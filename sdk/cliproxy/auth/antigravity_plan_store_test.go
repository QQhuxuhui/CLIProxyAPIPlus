package auth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type recordingAntigravityPlanStore struct {
	mu        sync.Mutex
	load      map[string]AntigravityPlanRecord
	saved     map[string]AntigravityPlanRecord
	saveCount int
	failSaves int
}

func (s *recordingAntigravityPlanStore) Load(context.Context) (map[string]AntigravityPlanRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneAntigravityPlanRecords(s.load), nil
}

func (s *recordingAntigravityPlanStore) Save(_ context.Context, plans map[string]AntigravityPlanRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveCount++
	if s.failSaves > 0 {
		s.failSaves--
		return errors.New("injected plan save failure")
	}
	s.saved = cloneAntigravityPlanRecords(plans)
	return nil
}

func (s *recordingAntigravityPlanStore) failNextSave() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failSaves++
}

func (s *recordingAntigravityPlanStore) snapshot() (map[string]AntigravityPlanRecord, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneAntigravityPlanRecords(s.saved), s.saveCount
}

func cloneAntigravityPlanRecords(records map[string]AntigravityPlanRecord) map[string]AntigravityPlanRecord {
	cloned := make(map[string]AntigravityPlanRecord, len(records))
	for authID, record := range records {
		cloned[authID] = record
	}
	return cloned
}

func TestFileAntigravityPlanStoreSaveLoadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "auths")
	store := NewFileAntigravityPlanStore(dir)
	wantUpdatedAt := time.Date(2026, time.July, 16, 1, 2, 3, 0, time.UTC)

	if errSave := store.Save(context.Background(), map[string]AntigravityPlanRecord{
		"auth-1": {
			PaidTierID: "pro",
			UpdatedAt:  wantUpdatedAt,
		},
	}); errSave != nil {
		t.Fatalf("Save() returned error: %v", errSave)
	}

	loaded, errLoad := store.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load() returned error: %v", errLoad)
	}
	got, ok := loaded["auth-1"]
	if !ok {
		t.Fatalf("Load() = %#v, want auth-1", loaded)
	}
	if got.PaidTierID != "pro" || !got.UpdatedAt.Equal(wantUpdatedAt) {
		t.Fatalf("loaded record = %#v, want pro at %v", got, wantUpdatedAt)
	}

	info, errStat := os.Stat(filepath.Join(dir, "antigravity-plans.aps"))
	if errStat != nil {
		t.Fatalf("stat plan file: %v", errStat)
	}
	if gotMode := info.Mode().Perm(); gotMode != 0o600 {
		t.Fatalf("plan file mode = %o, want 600", gotMode)
	}
	dirInfo, errDirStat := os.Stat(dir)
	if errDirStat != nil {
		t.Fatalf("stat plan directory: %v", errDirStat)
	}
	if gotMode := dirInfo.Mode().Perm(); gotMode != 0o700 {
		t.Fatalf("new plan directory mode = %o, want 700", gotMode)
	}
	data, errRead := os.ReadFile(filepath.Join(dir, antigravityPlanFileName))
	if errRead != nil {
		t.Fatalf("read plan envelope: %v", errRead)
	}
	var envelope antigravityPlanEnvelope
	if errUnmarshal := json.Unmarshal(data, &envelope); errUnmarshal != nil {
		t.Fatalf("unmarshal plan envelope: %v", errUnmarshal)
	}
	if envelope.Version != antigravityPlanVersion {
		t.Fatalf("plan envelope version = %d, want %d", envelope.Version, antigravityPlanVersion)
	}
	tempFiles, errGlob := filepath.Glob(filepath.Join(dir, antigravityPlanFileName+".*.tmp"))
	if errGlob != nil {
		t.Fatalf("glob plan temp files: %v", errGlob)
	}
	if len(tempFiles) != 0 {
		t.Fatalf("leftover plan temp files = %v", tempFiles)
	}
}

func TestAntigravityPlanRegistryPersistsFullSnapshots(t *testing.T) {
	store := &recordingAntigravityPlanStore{}
	if errConfigure := ConfigureAntigravityPlanStore(context.Background(), store); errConfigure != nil {
		t.Fatalf("ConfigureAntigravityPlanStore() returned error: %v", errConfigure)
	}
	t.Cleanup(func() {
		_ = ConfigureAntigravityPlanStore(context.Background(), nil)
	})

	updatedAt := time.Date(2026, time.July, 16, 2, 3, 4, 0, time.UTC)
	SetAntigravityDisplayPlan("auth-1", "pro", updatedAt)
	SetAntigravityDisplayPlan("auth-1", "pro", updatedAt.Add(time.Hour))
	SetAntigravityDisplayPlan("auth-2", "ultra", updatedAt)

	snapshot, saveCount := store.snapshot()
	if saveCount != 2 {
		t.Fatalf("Save() count = %d, want 2 after identical-tier de-duplication", saveCount)
	}
	if len(snapshot) != 2 || snapshot["auth-1"].PaidTierID != "pro" || snapshot["auth-2"].PaidTierID != "ultra" {
		t.Fatalf("saved snapshot = %#v, want both plans", snapshot)
	}

	DeleteAntigravityDisplayPlan("auth-1")
	snapshot, saveCount = store.snapshot()
	if saveCount != 3 {
		t.Fatalf("Save() count after delete = %d, want 3", saveCount)
	}
	if _, ok := snapshot["auth-1"]; ok || len(snapshot) != 1 {
		t.Fatalf("saved snapshot after delete = %#v, want auth-2 only", snapshot)
	}
}

func TestAntigravityPlanRegistryRetriesFailedIdenticalSet(t *testing.T) {
	store := &recordingAntigravityPlanStore{}
	if errConfigure := ConfigureAntigravityPlanStore(context.Background(), store); errConfigure != nil {
		t.Fatalf("ConfigureAntigravityPlanStore() returned error: %v", errConfigure)
	}
	t.Cleanup(func() {
		_ = ConfigureAntigravityPlanStore(context.Background(), nil)
	})
	store.failNextSave()

	SetAntigravityDisplayPlan("retry-set-auth", "pro", time.Now())
	SetAntigravityDisplayPlan("retry-set-auth", "pro", time.Now())

	snapshot, saveCount := store.snapshot()
	if saveCount != 2 || snapshot["retry-set-auth"].PaidTierID != "pro" {
		t.Fatalf("save attempts/snapshot = %d/%#v, want retried pro snapshot", saveCount, snapshot)
	}
}

func TestAntigravityPlanRegistryRetriesFailedDelete(t *testing.T) {
	store := &recordingAntigravityPlanStore{}
	if errConfigure := ConfigureAntigravityPlanStore(context.Background(), store); errConfigure != nil {
		t.Fatalf("ConfigureAntigravityPlanStore() returned error: %v", errConfigure)
	}
	t.Cleanup(func() {
		_ = ConfigureAntigravityPlanStore(context.Background(), nil)
	})
	SetAntigravityDisplayPlan("retry-delete-auth", "pro", time.Now())
	store.failNextSave()

	DeleteAntigravityDisplayPlan("retry-delete-auth")
	DeleteAntigravityDisplayPlan("retry-delete-auth")

	snapshot, saveCount := store.snapshot()
	if saveCount != 3 || len(snapshot) != 0 {
		t.Fatalf("save attempts/snapshot = %d/%#v, want retried empty snapshot", saveCount, snapshot)
	}
}

func TestSetAntigravityCreditsHintForwardsDisplayPlan(t *testing.T) {
	store := &recordingAntigravityPlanStore{}
	if errConfigure := ConfigureAntigravityPlanStore(context.Background(), store); errConfigure != nil {
		t.Fatalf("ConfigureAntigravityPlanStore() returned error: %v", errConfigure)
	}
	t.Cleanup(func() {
		_ = ConfigureAntigravityPlanStore(context.Background(), nil)
	})

	updatedAt := time.Date(2026, time.July, 16, 3, 4, 5, 0, time.UTC)
	SetAntigravityCreditsHint("hint-plan-auth", AntigravityCreditsHint{
		Known:      true,
		PaidTierID: " pro ",
		UpdatedAt:  updatedAt,
	})

	record, ok := GetAntigravityDisplayPlan("hint-plan-auth")
	if !ok || record.PaidTierID != "pro" || !record.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("display plan = %#v, %t; want normalized pro plan", record, ok)
	}
}

func TestConfigureAntigravityPlanStoreReplacesAndClearsState(t *testing.T) {
	first := &recordingAntigravityPlanStore{load: map[string]AntigravityPlanRecord{
		"restored-auth": {PaidTierID: "pro", UpdatedAt: time.Now()},
		"empty-tier":    {},
	}}
	if errConfigure := ConfigureAntigravityPlanStore(context.Background(), first); errConfigure != nil {
		t.Fatalf("configure first store: %v", errConfigure)
	}
	t.Cleanup(func() {
		_ = ConfigureAntigravityPlanStore(context.Background(), nil)
	})
	if record, ok := GetAntigravityDisplayPlan("restored-auth"); !ok || record.PaidTierID != "pro" {
		t.Fatalf("restored display plan = %#v, %t; want pro", record, ok)
	}
	if _, ok := GetAntigravityDisplayPlan("empty-tier"); ok {
		t.Fatal("empty restored tier should be dropped")
	}
	if _, ok := GetAntigravityCreditsHint("restored-auth"); ok {
		t.Fatal("restored display plan must not create a credits hint")
	}

	second := &recordingAntigravityPlanStore{load: map[string]AntigravityPlanRecord{
		"second-auth": {PaidTierID: "ultra", UpdatedAt: time.Now()},
	}}
	if errConfigure := ConfigureAntigravityPlanStore(context.Background(), second); errConfigure != nil {
		t.Fatalf("configure second store: %v", errConfigure)
	}
	if _, ok := GetAntigravityDisplayPlan("restored-auth"); ok {
		t.Fatal("store replacement retained stale plan")
	}
	if record, ok := GetAntigravityDisplayPlan("second-auth"); !ok || record.PaidTierID != "ultra" {
		t.Fatalf("replacement display plan = %#v, %t; want ultra", record, ok)
	}

	if errConfigure := ConfigureAntigravityPlanStore(context.Background(), nil); errConfigure != nil {
		t.Fatalf("clear store: %v", errConfigure)
	}
	if _, ok := GetAntigravityDisplayPlan("second-auth"); ok {
		t.Fatal("nil store did not clear display plans")
	}
}

func TestAntigravityPlanRegistryConcurrentUpdatesKeepFullSnapshot(t *testing.T) {
	store := &recordingAntigravityPlanStore{}
	if errConfigure := ConfigureAntigravityPlanStore(context.Background(), store); errConfigure != nil {
		t.Fatalf("ConfigureAntigravityPlanStore() returned error: %v", errConfigure)
	}
	t.Cleanup(func() {
		_ = ConfigureAntigravityPlanStore(context.Background(), nil)
	})

	var wg sync.WaitGroup
	for _, authID := range []string{"concurrent-1", "concurrent-2", "concurrent-3"} {
		authID := authID
		wg.Add(1)
		go func() {
			defer wg.Done()
			SetAntigravityDisplayPlan(authID, "pro", time.Now())
		}()
	}
	wg.Wait()

	snapshot, _ := store.snapshot()
	if len(snapshot) != 3 {
		t.Fatalf("concurrent snapshot = %#v, want three plans", snapshot)
	}
}

func TestFileAntigravityPlanStoreRejectsMalformedAndUnsupportedFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		data any
	}{
		{name: "malformed", data: []byte("{")},
		{name: "unsupported version", data: antigravityPlanEnvelope{Version: 99, Plans: map[string]AntigravityPlanRecord{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var data []byte
			switch value := tc.data.(type) {
			case []byte:
				data = value
			default:
				var errMarshal error
				data, errMarshal = json.Marshal(value)
				if errMarshal != nil {
					t.Fatalf("marshal fixture: %v", errMarshal)
				}
			}
			if errWrite := os.WriteFile(filepath.Join(dir, antigravityPlanFileName), data, 0o600); errWrite != nil {
				t.Fatalf("write fixture: %v", errWrite)
			}
			if _, errLoad := NewFileAntigravityPlanStore(dir).Load(context.Background()); errLoad == nil {
				t.Fatal("Load() error = nil, want invalid snapshot error")
			}
		})
	}
}
