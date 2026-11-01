package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	log "github.com/sirupsen/logrus"
)

const (
	accountConcurrencyStoreDir     = ".codex-account-concurrency"
	accountConcurrencyStoreFile    = "limits.json"
	accountConcurrencyStoreVersion = 1
)

type accountConcurrencyFile struct {
	Version int            `json:"version"`
	Limits  map[string]int `json:"limits"`
}

// accountConcurrencyStore keeps per-account limits apart from credential files. Readers take the
// immutable map without locking; writes are serialized here, never under a Manager or pool lock.
type accountConcurrencyStore struct {
	limits atomic.Pointer[map[string]int]
	mu     sync.Mutex
	dir    string
	// corrupt marks an unreadable file that the next write moves aside instead of overwriting.
	corrupt bool
	// fsync is replaceable so tests can fail a write deterministically.
	fsync func(*os.File) error
}

func newAccountConcurrencyStore() *accountConcurrencyStore {
	s := &accountConcurrencyStore{fsync: func(f *os.File) error { return f.Sync() }}
	s.limits.Store(&map[string]int{})
	return s
}

func (s *accountConcurrencyStore) path() string {
	return filepath.Join(s.dir, accountConcurrencyStoreDir, accountConcurrencyStoreFile)
}

// open loads limits for authDir once per directory. A missing file means no overrides. A corrupt
// file also means no overrides, but it stays in place until the next write renames it.
func (s *accountConcurrencyStore) open(authDir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if authDir == "" || authDir == s.dir {
		return
	}
	s.dir = authDir
	s.corrupt = false
	limits := map[string]int{}
	data, errRead := os.ReadFile(s.path())
	switch {
	case errRead == nil:
		parsed, errParse := parseAccountConcurrencyFile(data)
		if errParse != nil {
			s.corrupt = true
			log.WithError(errParse).Errorf("codex account concurrency: ignoring unreadable %s", s.path())
		} else {
			limits = parsed
		}
	case !errors.Is(errRead, os.ErrNotExist):
		s.corrupt = true
		log.WithError(errRead).Errorf("codex account concurrency: ignoring unreadable %s", s.path())
	}
	s.limits.Store(&limits)
}

func parseAccountConcurrencyFile(data []byte) (map[string]int, error) {
	var file accountConcurrencyFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	if file.Version != accountConcurrencyStoreVersion {
		return nil, fmt.Errorf("unsupported version %d", file.Version)
	}
	if file.Limits == nil {
		return map[string]int{}, nil
	}
	for id, limit := range file.Limits {
		if strings.TrimSpace(id) == "" || limit < 0 {
			return nil, fmt.Errorf("invalid limit for %q", id)
		}
	}
	return file.Limits, nil
}

func (s *accountConcurrencyStore) get(id string) (int, bool) {
	limit, ok := (*s.limits.Load())[id]
	return limit, ok
}

func (s *accountConcurrencyStore) snapshot() map[string]int { return *s.limits.Load() }

// update applies mutate to a private copy and publishes it only after the file is durable, so a
// failed write leaves memory and disk on the old values. mutate returns false to skip the write.
func (s *accountConcurrencyStore) update(mutate func(map[string]int) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := maps.Clone(*s.limits.Load())
	if !mutate(next) {
		return nil
	}
	if s.dir == "" {
		return errors.New("codex account concurrency: auth directory is not configured")
	}
	if err := s.writeLocked(next); err != nil {
		return err
	}
	s.corrupt = false
	s.limits.Store(&next)
	return nil
}

func (s *accountConcurrencyStore) writeLocked(limits map[string]int) (err error) {
	data, errMarshal := json.MarshalIndent(accountConcurrencyFile{Version: accountConcurrencyStoreVersion, Limits: limits}, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("codex account concurrency: encode limits: %w", errMarshal)
	}
	target := s.path()
	dir := filepath.Dir(target)
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return fmt.Errorf("codex account concurrency: create directory: %w", errMkdir)
	}
	temp, errTemp := os.CreateTemp(dir, accountConcurrencyStoreFile+".tmp-*")
	if errTemp != nil {
		return fmt.Errorf("codex account concurrency: create temp file: %w", errTemp)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(temp.Name())
		}
	}()
	_, errWrite := temp.Write(append(data, '\n'))
	if errWrite == nil {
		errWrite = s.fsync(temp)
	}
	if errClose := temp.Close(); errWrite == nil {
		errWrite = errClose
	}
	if errWrite != nil {
		return fmt.Errorf("codex account concurrency: write temp file: %w", errWrite)
	}
	bad := target + ".bad"
	if s.corrupt {
		if errBad := os.Rename(target, bad); errBad != nil && !errors.Is(errBad, os.ErrNotExist) {
			return fmt.Errorf("codex account concurrency: move corrupt file aside: %w", errBad)
		}
	}
	if errRename := os.Rename(temp.Name(), target); errRename != nil {
		if s.corrupt {
			_ = os.Rename(bad, target)
		}
		return fmt.Errorf("codex account concurrency: replace limits file: %w", errRename)
	}
	return nil
}
