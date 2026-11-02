package codexspeed

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// DirName is the hidden directory under the auth dir. Files avoid the .json suffix so
// credential loaders skip them.
const DirName = ".codex-speed"

const (
	historyFile     = "history.jsonl"
	transitionsFile = "transitions.jsonl"
	accountsFile    = "accounts.dat"
	// Retention bounds every persisted series.
	Retention = 7 * 24 * time.Hour
)

// Store persists hourly records under one directory.
type Store struct {
	dir string
	mu  sync.Mutex
}

// NewStore returns a store rooted at dir; the directory is created on first write.
func NewStore(dir string) *Store { return &Store{dir: dir} }

// LoadHours reads history.jsonl, skipping malformed lines and records older than Retention.
func (s *Store) LoadHours(now time.Time) []HourRecord {
	if s == nil || s.dir == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadHoursLocked(now)
}

func (s *Store) loadHoursLocked(now time.Time) []HourRecord {
	data, err := os.ReadFile(filepath.Join(s.dir, historyFile))
	if err != nil {
		if !os.IsNotExist(err) {
			log.WithError(err).Warn("codex speed: read history failed")
		}
		return nil
	}
	cutoff := now.Add(-Retention)
	var out []HourRecord
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	bad := 0
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec HourRecord
		if errUnmarshal := json.Unmarshal(line, &rec); errUnmarshal != nil || rec.Hour.IsZero() {
			bad++
			continue
		}
		if rec.Hour.Before(cutoff) {
			continue
		}
		out = append(out, rec)
	}
	if bad > 0 {
		log.Warnf("codex speed: skipped %d malformed history lines", bad)
	}
	return out
}

// AppendHour adds rec and rewrites the file without records older than Retention.
func (s *Store) AppendHour(rec HourRecord, now time.Time) error {
	if s == nil || s.dir == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records := append(s.loadHoursLocked(now), rec)
	var buf bytes.Buffer
	for _, r := range records {
		line, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("codex speed: encode history: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return writeFileAtomic(s.dir, historyFile, buf.Bytes())
}

func writeFileAtomic(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("codex speed: create dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return fmt.Errorf("codex speed: create temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, errWrite := tmp.Write(data); errWrite != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("codex speed: write temp: %w", errWrite)
	}
	if errClose := tmp.Close(); errClose != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("codex speed: close temp: %w", errClose)
	}
	if errRename := os.Rename(tmpName, filepath.Join(dir, name)); errRename != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("codex speed: replace %s: %w", name, errRename)
	}
	return nil
}

// AppendTransitions appends tier changes; lines older than Retention are dropped by PruneTransitions.
func (s *Store) AppendTransitions(items []Transition) error {
	if s == nil || s.dir == "" || len(items) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("codex speed: create dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(s.dir, transitionsFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("codex speed: open transitions: %w", err)
	}
	var buf bytes.Buffer
	for _, item := range items {
		line, errMarshal := json.Marshal(item)
		if errMarshal != nil {
			_ = f.Close()
			return fmt.Errorf("codex speed: encode transition: %w", errMarshal)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if _, errWrite := f.Write(buf.Bytes()); errWrite != nil {
		_ = f.Close()
		return fmt.Errorf("codex speed: write transitions: %w", errWrite)
	}
	return f.Close()
}

// PruneTransitions rewrites transitions.jsonl without lines older than Retention or malformed.
func (s *Store) PruneTransitions(now time.Time) error {
	if s == nil || s.dir == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(s.dir, transitionsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("codex speed: read transitions: %w", err)
	}
	cutoff := now.Add(-Retention)
	var buf bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		var item Transition
		if len(line) == 0 || json.Unmarshal(line, &item) != nil || item.Time.Before(cutoff) {
			continue
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return writeFileAtomic(s.dir, transitionsFile, buf.Bytes())
}

// SaveAccounts replaces accounts.dat with data.
func (s *Store) SaveAccounts(data []byte) error {
	if s == nil || s.dir == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeFileAtomic(s.dir, accountsFile, data)
}

// LoadAccounts returns accounts.dat, or nil when it is missing or unreadable.
func (s *Store) LoadAccounts() []byte {
	if s == nil || s.dir == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(s.dir, accountsFile))
	if err != nil {
		if !os.IsNotExist(err) {
			log.WithError(err).Warn("codex speed: read accounts state failed")
		}
		return nil
	}
	return data
}
