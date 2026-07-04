// Package usagestats persists per-account per-model call counts by day and
// exposes range queries. It registers a coreusage plugin that aggregates
// completed requests in memory and periodically flushes to per-day JSON files.
package usagestats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Counts holds success/fail call counts for one (account, model, day).
type Counts struct {
	Success int64 `json:"success"`
	Fail    int64 `json:"fail"`
}

// dayData is the on-disk shape: one file per day.
type dayData struct {
	Date     string                        `json:"date"`
	Accounts map[string]map[string]*Counts `json:"accounts"` // authIndex -> model -> counts
}

func newDayData(day string) *dayData {
	return &dayData{Date: day, Accounts: make(map[string]map[string]*Counts)}
}

type store struct {
	dir string
}

func newStore(dir string) *store { return &store{dir: dir} }

func (s *store) path(day string) string { return filepath.Join(s.dir, day+".json") }

// readFileExists reports whether a path exists (test + sweep helper).
func readFileExists(path string) (os.FileInfo, error) { return os.Stat(path) }

func (s *store) load(day string) (*dayData, error) {
	raw, err := os.ReadFile(s.path(day))
	if err != nil {
		if os.IsNotExist(err) {
			return newDayData(day), nil
		}
		return nil, err
	}
	d := newDayData(day)
	if err := json.Unmarshal(raw, d); err != nil {
		return nil, err
	}
	if d.Accounts == nil {
		d.Accounts = make(map[string]map[string]*Counts)
	}
	return d, nil
}

func (s *store) save(day string, d *dayData) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	tmp := s.path(day) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(day))
}

// sweep deletes day files older than retentionDays.
func (s *store) sweep(now time.Time, retentionDays int) {
	if retentionDays <= 0 {
		return
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	cutoff := now.AddDate(0, 0, -retentionDays)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		day := strings.TrimSuffix(name, ".json")
		t, perr := time.ParseInLocation("2006-01-02", day, time.Local)
		if perr != nil {
			continue
		}
		if t.Before(cutoff) {
			_ = os.Remove(filepath.Join(s.dir, name))
		}
	}
}
