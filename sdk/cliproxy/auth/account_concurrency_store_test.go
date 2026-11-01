package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func storeFile(dir string) string {
	return filepath.Join(dir, accountConcurrencyStoreDir, accountConcurrencyStoreFile)
}

func openStore(t *testing.T, dir string) *accountConcurrencyStore {
	t.Helper()
	s := newAccountConcurrencyStore()
	s.open(dir)
	return s
}

func setLimit(s *accountConcurrencyStore, id string, limit int) error {
	return s.update(func(limits map[string]int) bool { limits[id] = limit; return true })
}

func TestAccountConcurrencyStoreSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	if err := setLimit(s, "a", 8); err != nil {
		t.Fatal(err)
	}
	if err := setLimit(s, "b", 0); err != nil {
		t.Fatal(err)
	}
	reopened := openStore(t, dir)
	if limit, ok := reopened.get("a"); !ok || limit != 8 {
		t.Fatalf("a = %d %t", limit, ok)
	}
	if limit, ok := reopened.get("b"); !ok || limit != 0 {
		t.Fatalf("b = %d %t", limit, ok)
	}
	if _, ok := reopened.get("c"); ok {
		t.Fatal("missing account must inherit")
	}
	data, err := os.ReadFile(storeFile(dir))
	if err != nil || !strings.Contains(string(data), `"version": 1`) {
		t.Fatalf("file = %s %v", data, err)
	}
}

func TestAccountConcurrencyStoreLimitsApplyThroughManager(t *testing.T) {
	dir := t.TempDir()
	cfg := concurrencyTestConfig(true, 2, false)
	cfg.AuthDir = dir
	m := NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	one, zero := 1, 0
	if _, err := m.SetCodexAccountConcurrencyLimits([]string{"one"}, &one); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetCodexAccountConcurrencyLimits([]string{"zero"}, &zero); err != nil {
		t.Fatal(err)
	}
	restarted := NewManager(nil, nil, nil)
	restarted.SetConfig(cfg)
	if got := restarted.accountConcurrency.limitFor("one"); got != 1 {
		t.Fatalf("override = %d", got)
	}
	if got := restarted.accountConcurrency.limitFor("zero"); got != 0 {
		t.Fatalf("unlimited = %d", got)
	}
	if got := restarted.accountConcurrency.limitFor("other"); got != 2 {
		t.Fatalf("default = %d", got)
	}
	restarted.SetConfig(cfg)
	if got := restarted.accountConcurrency.limitFor("one"); got != 1 {
		t.Fatalf("hot reload lost override: %d", got)
	}
	result, err := restarted.SetCodexAccountConcurrencyLimits([]string{"one", "other"}, nil)
	if err != nil || len(result.Changed) != 1 || len(result.Unchanged) != 1 {
		t.Fatalf("restore = %+v %v", result, err)
	}
	if got := restarted.accountConcurrency.limitFor("one"); got != 2 {
		t.Fatalf("restored = %d", got)
	}
}

func TestAccountConcurrencyStoreWriteFailureKeepsOldValues(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	if err := setLimit(s, "a", 1); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(storeFile(dir))
	if err != nil {
		t.Fatal(err)
	}
	s.fsync = func(*os.File) error { return errors.New("disk full") }
	if err = setLimit(s, "a", 9); err == nil {
		t.Fatal("expected a write failure")
	}
	if limit, _ := s.get("a"); limit != 1 {
		t.Fatalf("memory changed: %d", limit)
	}
	after, err := os.ReadFile(storeFile(dir))
	if err != nil || string(after) != string(before) {
		t.Fatalf("file changed: %s %v", after, err)
	}
	entries, err := os.ReadDir(filepath.Dir(storeFile(dir)))
	if err != nil || len(entries) != 1 {
		t.Fatalf("temp file left behind: %v %v", entries, err)
	}
}

func TestAccountConcurrencyStoreCorruptFileIsPreservedUntilNextWrite(t *testing.T) {
	for name, content := range map[string]string{
		"not json":     "{broken",
		"bad version":  `{"version":2,"limits":{}}`,
		"negative":     `{"version":1,"limits":{"a":-1}}`,
		"non-integer":  `{"version":1,"limits":{"a":1.5}}`,
		"empty object": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := storeFile(dir)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			s := openStore(t, dir)
			if len(s.snapshot()) != 0 {
				t.Fatalf("limits = %v", s.snapshot())
			}
			if data, _ := os.ReadFile(path); string(data) != content {
				t.Fatal("opening overwrote the corrupt file")
			}
			if err := setLimit(s, "b", 3); err != nil {
				t.Fatal(err)
			}
			bad, err := os.ReadFile(path + ".bad")
			if err != nil || string(bad) != content {
				t.Fatalf("bad copy = %s %v", bad, err)
			}
			if limit, ok := openStore(t, dir).get("b"); !ok || limit != 3 {
				t.Fatalf("new file = %d %t", limit, ok)
			}
		})
	}
}

func TestAccountConcurrencyStoreBatchWritesOnce(t *testing.T) {
	cfg := concurrencyTestConfig(true, 2, false)
	cfg.AuthDir = t.TempDir()
	m := NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	writes := 0
	m.accountConcurrency.store.fsync = func(f *os.File) error { writes++; return f.Sync() }
	ids := make([]string, 3000)
	for i := range ids {
		ids[i] = fmt.Sprintf("account-%d", i)
	}
	limit := 8
	result, err := m.SetCodexAccountConcurrencyLimits(ids, &limit)
	if err != nil || len(result.Changed) != 3000 || writes != 1 {
		t.Fatalf("changed=%d writes=%d err=%v", len(result.Changed), writes, err)
	}
	if result, err = m.SetCodexAccountConcurrencyLimits(ids, &limit); err != nil || len(result.Unchanged) != 3000 || writes != 1 {
		t.Fatalf("unchanged=%d writes=%d err=%v", len(result.Unchanged), writes, err)
	}
}

func TestAccountConcurrencyLimitsDoNotTouchAccounts(t *testing.T) {
	cfg := concurrencyTestConfig(true, 2, false)
	cfg.AuthDir = t.TempDir()
	m := NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	if _, err := m.Register(context.Background(), oauthCodex("a")); err != nil {
		t.Fatal(err)
	}
	before, _ := m.GetByID("a")
	limit := 5
	if _, err := m.SetCodexAccountConcurrencyLimits([]string{"a"}, &limit); err != nil {
		t.Fatal(err)
	}
	after, _ := m.GetByID("a")
	if after.Generation != before.Generation || !after.UpdatedAt.Equal(before.UpdatedAt) || after.Metadata["concurrency_limit"] != nil {
		t.Fatalf("account changed: %+v", after)
	}
}

func TestAccountConcurrencyStoreWithoutAuthDirRejectsWrites(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetConfig(&internalconfig.Config{})
	limit := 1
	if _, err := m.SetCodexAccountConcurrencyLimits([]string{"a"}, &limit); err == nil {
		t.Fatal("expected an error")
	}
	if got := m.accountConcurrency.limitFor("a"); got != 2 {
		t.Fatalf("limit = %d", got)
	}
}
