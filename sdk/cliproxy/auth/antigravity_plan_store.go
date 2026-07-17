package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	antigravityPlanFileName = "antigravity-plans.aps"
	antigravityPlanVersion  = 1
)

// AntigravityPlanRecord is the persisted display plan for one auth.
type AntigravityPlanRecord struct {
	PaidTierID string    `json:"paid_tier_id"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// AntigravityPlanStore persists display plans independently from credits hints.
type AntigravityPlanStore interface {
	Load(context.Context) (map[string]AntigravityPlanRecord, error)
	Save(context.Context, map[string]AntigravityPlanRecord) error
}

type antigravityPlanEnvelope struct {
	Version int                              `json:"version"`
	Plans   map[string]AntigravityPlanRecord `json:"plans"`
}

// FileAntigravityPlanStore stores all display plans in one atomic snapshot.
type FileAntigravityPlanStore struct {
	mu  sync.Mutex
	dir string
}

var antigravityPlanRegistry = struct {
	sync.Mutex
	store AntigravityPlanStore
	plans map[string]AntigravityPlanRecord
	dirty bool
}{
	plans: make(map[string]AntigravityPlanRecord),
}

// NewFileAntigravityPlanStore creates a plan store rooted at dir.
func NewFileAntigravityPlanStore(dir string) *FileAntigravityPlanStore {
	return &FileAntigravityPlanStore{dir: strings.TrimSpace(dir)}
}

// Load reads the current display-plan snapshot.
func (s *FileAntigravityPlanStore) Load(ctx context.Context) (map[string]AntigravityPlanRecord, error) {
	if s == nil || s.dir == "" {
		return map[string]AntigravityPlanRecord{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return nil, errCtx
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data, errRead := os.ReadFile(filepath.Join(s.dir, antigravityPlanFileName))
	if errors.Is(errRead, os.ErrNotExist) {
		return map[string]AntigravityPlanRecord{}, nil
	}
	if errRead != nil {
		return nil, fmt.Errorf("read antigravity plan snapshot: %w", errRead)
	}
	var envelope antigravityPlanEnvelope
	if errUnmarshal := json.Unmarshal(data, &envelope); errUnmarshal != nil {
		return nil, fmt.Errorf("parse antigravity plan snapshot: %w", errUnmarshal)
	}
	if envelope.Version != antigravityPlanVersion {
		return nil, fmt.Errorf("unsupported antigravity plan snapshot version %d", envelope.Version)
	}
	if envelope.Plans == nil {
		envelope.Plans = make(map[string]AntigravityPlanRecord)
	}
	return envelope.Plans, nil
}

// Save atomically replaces the display-plan snapshot.
func (s *FileAntigravityPlanStore) Save(ctx context.Context, plans map[string]AntigravityPlanRecord) error {
	if s == nil || s.dir == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errCtx := ctx.Err(); errCtx != nil {
		return errCtx
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if errMkdir := os.MkdirAll(s.dir, 0o700); errMkdir != nil {
		return fmt.Errorf("create antigravity plan directory: %w", errMkdir)
	}
	data, errMarshal := json.MarshalIndent(antigravityPlanEnvelope{
		Version: antigravityPlanVersion,
		Plans:   plans,
	}, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("marshal antigravity plan snapshot: %w", errMarshal)
	}
	data = append(data, '\n')

	tmpFile, errCreate := os.CreateTemp(s.dir, antigravityPlanFileName+".*.tmp")
	if errCreate != nil {
		return fmt.Errorf("create antigravity plan temp file: %w", errCreate)
	}
	tmpPath := tmpFile.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if errChmod := tmpFile.Chmod(0o600); errChmod != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("chmod antigravity plan temp file: %w", errChmod)
	}
	if _, errWrite := tmpFile.Write(data); errWrite != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write antigravity plan temp file: %w", errWrite)
	}
	if errClose := tmpFile.Close(); errClose != nil {
		return fmt.Errorf("close antigravity plan temp file: %w", errClose)
	}
	if errRename := os.Rename(tmpPath, filepath.Join(s.dir, antigravityPlanFileName)); errRename != nil {
		return fmt.Errorf("replace antigravity plan snapshot: %w", errRename)
	}
	removeTemp = false
	return nil
}

// ConfigureAntigravityPlanStore replaces the store and all loaded display plans.
func ConfigureAntigravityPlanStore(ctx context.Context, store AntigravityPlanStore) error {
	if ctx == nil {
		ctx = context.Background()
	}
	antigravityPlanRegistry.Lock()
	defer antigravityPlanRegistry.Unlock()

	antigravityPlanRegistry.store = store
	antigravityPlanRegistry.plans = make(map[string]AntigravityPlanRecord)
	antigravityPlanRegistry.dirty = false
	if store == nil {
		return nil
	}
	loaded, errLoad := store.Load(ctx)
	if errLoad != nil {
		return errLoad
	}
	for authID, record := range loaded {
		authID = strings.TrimSpace(authID)
		record.PaidTierID = strings.TrimSpace(record.PaidTierID)
		if authID == "" || record.PaidTierID == "" {
			continue
		}
		antigravityPlanRegistry.plans[authID] = record
	}
	return nil
}

// SetAntigravityDisplayPlan stores a confirmed non-empty plan for an auth.
func SetAntigravityDisplayPlan(authID, paidTierID string, updatedAt time.Time) {
	authID = strings.TrimSpace(authID)
	paidTierID = strings.TrimSpace(paidTierID)
	if authID == "" || paidTierID == "" {
		return
	}
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}

	antigravityPlanRegistry.Lock()
	defer antigravityPlanRegistry.Unlock()
	if existing, ok := antigravityPlanRegistry.plans[authID]; ok && existing.PaidTierID == paidTierID {
		if antigravityPlanRegistry.dirty {
			persistAntigravityPlansLocked(authID)
		}
		return
	}
	antigravityPlanRegistry.plans[authID] = AntigravityPlanRecord{
		PaidTierID: paidTierID,
		UpdatedAt:  updatedAt,
	}
	antigravityPlanRegistry.dirty = true
	persistAntigravityPlansLocked(authID)
}

// GetAntigravityDisplayPlan returns the last confirmed display plan for an auth.
func GetAntigravityDisplayPlan(authID string) (AntigravityPlanRecord, bool) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return AntigravityPlanRecord{}, false
	}
	antigravityPlanRegistry.Lock()
	defer antigravityPlanRegistry.Unlock()
	record, ok := antigravityPlanRegistry.plans[authID]
	return record, ok
}

// DeleteAntigravityDisplayPlan removes a persisted display plan.
func DeleteAntigravityDisplayPlan(authID string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	antigravityPlanRegistry.Lock()
	defer antigravityPlanRegistry.Unlock()
	if _, ok := antigravityPlanRegistry.plans[authID]; !ok {
		if antigravityPlanRegistry.dirty {
			persistAntigravityPlansLocked(authID)
		}
		return
	}
	delete(antigravityPlanRegistry.plans, authID)
	antigravityPlanRegistry.dirty = true
	persistAntigravityPlansLocked(authID)
}

func persistAntigravityPlansLocked(authID string) {
	if antigravityPlanRegistry.store == nil {
		antigravityPlanRegistry.dirty = false
		return
	}
	snapshot := make(map[string]AntigravityPlanRecord, len(antigravityPlanRegistry.plans))
	for id, record := range antigravityPlanRegistry.plans {
		snapshot[id] = record
	}
	if errSave := antigravityPlanRegistry.store.Save(context.Background(), snapshot); errSave != nil {
		antigravityPlanRegistry.dirty = true
		log.WithField("auth_id", authID).Warnf("failed to persist antigravity display plan: %v", errSave)
		return
	}
	antigravityPlanRegistry.dirty = false
}
