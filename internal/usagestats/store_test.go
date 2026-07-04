package usagestats

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStore_SaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := newStore(dir)
	d := &dayData{Date: "2026-07-03", Accounts: map[string]map[string]*Counts{
		"1": {"gemini-3-pro": {Success: 120, Fail: 3}},
	}}
	if err := s.save("2026-07-03", d); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.load("2026-07-03")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	c := got.Accounts["1"]["gemini-3-pro"]
	if c == nil || c.Success != 120 || c.Fail != 3 {
		t.Errorf("round-trip = %+v, want success 120 fail 3", c)
	}
}

func TestStore_LoadMissingIsEmpty(t *testing.T) {
	s := newStore(t.TempDir())
	got, err := s.load("2000-01-01")
	if err != nil {
		t.Fatalf("load missing must not error: %v", err)
	}
	if len(got.Accounts) != 0 {
		t.Errorf("missing day should be empty, got %d accounts", len(got.Accounts))
	}
}

func TestStore_SweepDeletesOldFiles(t *testing.T) {
	dir := t.TempDir()
	s := newStore(dir)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)
	now := time.Date(2026, 7, 3, 0, 0, 0, 0, time.Local)
	if err := s.save(old.Format("2006-01-02"), &dayData{Date: "2026-01-01", Accounts: map[string]map[string]*Counts{}}); err != nil {
		t.Fatal(err)
	}
	if err := s.save(now.Format("2006-01-02"), &dayData{Date: "2026-07-03", Accounts: map[string]map[string]*Counts{}}); err != nil {
		t.Fatal(err)
	}
	s.sweep(now, 90)
	if _, err := s.load("2026-01-01"); err != nil {
		t.Fatal(err)
	}
	// old file must be gone from disk (load returns empty, but the file itself removed):
	if _, statErr := readFileExists(filepath.Join(dir, "2026-01-01.json")); statErr == nil {
		t.Error("old file should have been swept")
	}
	if _, statErr := readFileExists(filepath.Join(dir, "2026-07-03.json")); statErr != nil {
		t.Error("recent file must be kept")
	}
}
