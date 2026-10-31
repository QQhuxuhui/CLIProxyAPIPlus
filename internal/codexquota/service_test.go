package codexquota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type fakeSource struct {
	observations []coreauth.CodexQuotaObservation
	snapshot     coreauth.CodexPoolSnapshot
	home         bool
}

func (f *fakeSource) CodexQuotaObservations() []coreauth.CodexQuotaObservation {
	return f.observations
}
func (f *fakeSource) CodexPoolSnapshot() coreauth.CodexPoolSnapshot { return f.snapshot }
func (f *fakeSource) HomeEnabled() bool                             { return f.home }

func signals(used float64, minutes int, resetAt time.Time, plan string) map[string]string {
	return map[string]string{
		"X-Codex-Plan-Type":              plan,
		"X-Codex-Primary-Used-Percent":   fmt.Sprint(used),
		"X-Codex-Primary-Window-Minutes": fmt.Sprint(minutes),
		"X-Codex-Primary-Reset-At":       fmt.Sprint(resetAt.Unix()),
	}
}

func newTestService(t *testing.T, source *fakeSource, probe ProbeFunc, now *time.Time) *Service {
	t.Helper()
	s := New(t.TempDir(), func() Source { return source }, probe)
	s.now = func() time.Time { return *now }
	s.startedAt = now.Add(-time.Hour)
	return s
}

func TestWindowsFromSignalsSkipsEmptySecondary(t *testing.T) {
	observed := time.Unix(1000, 0)
	windows, plan := WindowsFromSignals(map[string]string{
		"X-Codex-Plan-Type":                   "Plus",
		"X-Codex-Primary-Used-Percent":        "40",
		"X-Codex-Primary-Window-Minutes":      "300",
		"X-Codex-Primary-Reset-After-Seconds": "60",
		"X-Codex-Secondary-Used-Percent":      "0",
		"X-Codex-Secondary-Window-Minutes":    "0",
	}, observed)
	if plan != "plus" || len(windows) != 1 {
		t.Fatalf("plan=%q windows=%v", plan, windows)
	}
	if !windows[0].ResetAt.Equal(observed.Add(time.Minute)) {
		t.Fatalf("reset = %v", windows[0].ResetAt)
	}
}

func TestParseUsageFreeAndLimitReached(t *testing.T) {
	now := time.Unix(1000, 0)
	windows, plan, errParse := ParseUsage([]byte(`{"plan_type":"free","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":12.5,"limit_window_seconds":2592000,"reset_at":5000},"secondary_window":null}}`), now)
	if errParse != nil || plan != "free" || len(windows) != 1 || windows[0].Minutes != 43200 || windows[0].UsedPercent != 12.5 {
		t.Fatalf("windows=%v plan=%q err=%v", windows, plan, errParse)
	}
	windows, _, errParse = ParseUsage([]byte(`{"rate_limit":{"limit_reached":true,"primary_window":{"limit_window_seconds":18000,"reset_after_seconds":60}}}`), now)
	if errParse != nil || windows[0].UsedPercent != 100 {
		t.Fatalf("limit reached without percent: %v err=%v", windows, errParse)
	}
	if _, _, errParse = ParseUsage([]byte(`{"rate_limit":{}}`), now); errParse == nil {
		t.Fatal("expected error for response without windows")
	}
}

func TestSummaryDedupesTightestWindowAndKeepsUnknownSeparate(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	future := now.Add(time.Hour)
	week := now.Add(100 * time.Hour)
	accounts := map[string]*Account{
		"a1": {ID: "a1", Name: "a1", AccountID: "x", Plan: "plus", ObservedAt: now, Windows: []Window{{Minutes: 300, UsedPercent: 95, ResetAt: future}, {Minutes: 10080, UsedPercent: 40, ResetAt: week}}},
		"a2": {ID: "a2", Name: "a2", AccountID: "x", Plan: "plus", ObservedAt: now.Add(-time.Hour), Windows: []Window{{Minutes: 300, UsedPercent: 10, ResetAt: future}}},
		"b":  {ID: "b", Name: "b", AccountID: "y", Plan: "plus"},
		"c":  {ID: "c", Name: "c", AccountID: "z", Plan: "plus", Disabled: true},
		"d":  {ID: "d", Name: "d", AccountID: "w", Plan: "free", credentialInvalid: true},
	}
	got := buildSummary(accounts, &history{}, capacityInput{Capacity: 20, Schedulable: 1, FallbackLimit: 20}, DefaultSettings(), now)
	if got.Files != 5 || got.UniqueAccounts != 2 || got.DuplicateFiles != 1 || got.Disabled != 1 || got.Invalid != 1 {
		t.Fatalf("counts = %+v", got)
	}
	if len(got.Plans) != 1 || got.Plans[0].Known != 1 || got.Plans[0].Unknown != 1 {
		t.Fatalf("plans = %+v", got.Plans)
	}
	plan := got.Plans[0]
	// a1 has 5% left on its tightest window, which is the usable floor.
	if plan.AvgRemaining != 5 || plan.Usable != 1 || plan.Distribution[0] != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	if !got.Advice.WarmingUp {
		t.Fatal("expected warming up without history")
	}
	if got.Advice.UsableCapacity != 40 {
		t.Fatalf("usable capacity = %d", got.Advice.UsableCapacity)
	}
}

func TestPassedResetCountsAsFullAndPending(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	accounts := map[string]*Account{"a": {ID: "a", Name: "a", Plan: "free", Windows: []Window{{Minutes: 43200, UsedPercent: 100, ResetAt: now.Add(-time.Minute)}}}}
	got := buildSummary(accounts, &history{}, capacityInput{FallbackLimit: 20}, DefaultSettings(), now)
	if got.Plans[0].Pending != 1 || got.Plans[0].AvgRemaining != 100 || got.Usable != 1 {
		t.Fatalf("plan = %+v", got.Plans[0])
	}
	row := buildRows(accounts, now)["a"]
	if !row.Windows[0].Pending || row.Windows[0].UsedPercent != 0 || *row.Remaining != 100 {
		t.Fatalf("row = %+v", row)
	}
}

func TestForecastRefillsOnResetAndReportsShortfall(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	// One account 50% left: 10/h empties it after 5 hours with nothing refilling.
	runway, shortfall := forecast([]forecastInput{{used: 50}}, 10, now, 10080, 24)
	if runway == nil || *runway != 5 || shortfall != 2 {
		t.Fatalf("runway=%v shortfall=%d", runway, shortfall)
	}
	// A reset at hour 2 returns the 50% and pushes exhaustion to hour 10.
	runway, _ = forecast([]forecastInput{{used: 50, resetAt: now.Add(2 * time.Hour)}}, 10, now, 10080, 24)
	if runway == nil || *runway != 10 {
		t.Fatalf("runway with reset = %v", runway)
	}
	// Short windows only need to survive their own length.
	_, shortfall = forecast([]forecastInput{{used: 0}}, 10, now, 300, 24)
	if shortfall != 0 {
		t.Fatalf("5h window shortfall = %d", shortfall)
	}
	if runway, shortfall = forecast([]forecastInput{{used: 50}}, 0, now, 10080, 24); runway != nil || shortfall != 0 {
		t.Fatalf("no burn: runway=%v shortfall=%d", runway, shortfall)
	}
}

func TestRecommendationTakesLargerOfRPMAndQuotaGap(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	accounts := map[string]*Account{}
	for i := range 3 {
		id := fmt.Sprint("a", i)
		accounts[id] = &Account{ID: id, Name: id, AccountID: id, Plan: "free", Windows: []Window{{Minutes: 43200, UsedPercent: 50, ResetAt: now.Add(600 * time.Hour)}}}
	}
	hist := &history{}
	for i := range 288 {
		at := now.Add(-time.Duration(287-i) * sampleInterval)
		hist.samples = append(hist.samples, sample{Time: at, PeakRPM: 100, Burn: map[string]float64{burnKey("free", 43200): 5}})
	}
	got := buildSummary(accounts, hist, capacityInput{Capacity: 60, Schedulable: 3, FallbackLimit: 20}, DefaultSettings(), now)
	// 3 × 20 = 60 RPM against 120 target needs 3 more accounts.
	if got.Advice.PeakRPM != 100 || got.Advice.TargetRPM != 120 || got.Advice.RPMShortfall != 3 {
		t.Fatalf("advice = %+v", got.Advice)
	}
	// 60 units/h for 24h against 150 left needs 1290/100 → 13 accounts.
	if got.Advice.QuotaShortfall != 13 || got.Advice.RecommendedAccounts != 13 {
		t.Fatalf("advice = %+v", got.Advice)
	}
	if got.Advice.WarmingUp {
		t.Fatal("24h of samples should not be warming up")
	}
	if got.Advice.RunwayHours == nil || *got.Advice.RunwayHours != 3 {
		t.Fatalf("runway = %v", got.Advice.RunwayHours)
	}
}

func TestMergeCreditsBurnAcrossObservationsAndWindowRollover(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	reset := now.Add(time.Hour)
	source := &fakeSource{}
	s := newTestService(t, source, nil, &now)
	observe := func(used float64, at, resetAt time.Time) {
		source.observations = []coreauth.CodexQuotaObservation{{ID: "a", Name: "a", AccountID: "x", ObservedAt: at, Signals: signals(used, 300, resetAt, "plus")}}
		s.merge(source.CodexQuotaObservations())
	}
	observe(10, now, reset)
	observe(25, now.Add(time.Minute), reset)
	if got := s.burn[burnKey("plus", 300)]; got != 15 {
		t.Fatalf("burn after increase = %v", got)
	}
	observe(25, now.Add(2*time.Minute), reset.Add(time.Second))
	observe(5, now.Add(3*time.Minute), reset.Add(5*time.Hour))
	if got := s.burn[burnKey("plus", 300)]; got != 20 {
		t.Fatalf("burn after rollover = %v", got)
	}
}

func TestDuplicateFilesReportingOutOfOrderDoNotDoubleCountBurn(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	reset := now.Add(time.Hour)
	source := &fakeSource{}
	s := newTestService(t, source, nil, &now)
	for _, step := range []struct {
		id   string
		used float64
	}{{"a", 50}, {"b", 45}, {"a", 51}} {
		now = now.Add(time.Minute)
		source.observations = []coreauth.CodexQuotaObservation{{ID: step.id, Name: step.id, AccountID: "same", ObservedAt: now, Signals: signals(step.used, 300, reset, "plus")}}
		s.merge(source.observations)
	}
	if got := s.burn[burnKey("plus", 300)]; got != 1 {
		t.Fatalf("burn = %v, want 1", got)
	}
}

func TestMergeKeepsCacheDuringStartupGrace(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	source := &fakeSource{}
	s := newTestService(t, source, nil, &now)
	s.accounts["gone"] = &Account{ID: "gone", Name: "gone"}
	s.startedAt = now
	s.merge([]coreauth.CodexQuotaObservation{{ID: "a", Name: "a"}})
	if s.accounts["gone"] == nil {
		t.Fatal("cache entry removed during startup grace")
	}
	s.startedAt = now.Add(-removalGrace)
	s.merge([]coreauth.CodexQuotaObservation{{ID: "a", Name: "a"}})
	if s.accounts["gone"] != nil {
		t.Fatal("removed account kept after grace")
	}
}

func TestCandidatesSkipExhaustedFreshAndDuplicate(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	s := newTestService(t, &fakeSource{}, nil, &now)
	old := now.Add(-2 * time.Hour)
	s.accounts = map[string]*Account{
		"never":     {ID: "never", AccountID: "1"},
		"stale":     {ID: "stale", AccountID: "2", ObservedAt: old, Windows: []Window{{Minutes: 300, UsedPercent: 10, ResetAt: now.Add(time.Hour)}}},
		"fresh":     {ID: "fresh", AccountID: "3", ObservedAt: now, ScanAt: old, Windows: []Window{{Minutes: 300, UsedPercent: 10, ResetAt: now.Add(time.Hour)}}},
		"exhausted": {ID: "exhausted", AccountID: "4", ObservedAt: old, ScanAt: old, Windows: []Window{{Minutes: 10080, UsedPercent: 100, ResetAt: now.Add(time.Hour)}}},
		"reset-due": {ID: "reset-due", AccountID: "9", ObservedAt: old, ScanAt: now.Add(-resetRecheck), Windows: []Window{{Minutes: 10080, UsedPercent: 100, ResetAt: now.Add(time.Hour)}}},
		"busy":      {ID: "busy", AccountID: "10", ObservedAt: now, Windows: []Window{{Minutes: 300, UsedPercent: 10, ResetAt: now.Add(time.Hour)}}},
		"recovered": {ID: "recovered", AccountID: "5", ObservedAt: now, Windows: []Window{{Minutes: 10080, UsedPercent: 100, ResetAt: now.Add(-time.Minute)}}},
		"disabled":  {ID: "disabled", AccountID: "6", Disabled: true},
		"denied":    {ID: "denied", AccountID: "7", ScanDenied: true, ScanAt: now.Add(-time.Hour)},
		"dup-a":     {ID: "dup-a", AccountID: "8"},
		"dup-b":     {ID: "dup-b", AccountID: "8"},
	}
	got := map[string]bool{}
	for _, id := range s.candidatesLocked(now) {
		got[id] = true
	}
	want := []string{"never", "stale", "recovered", "dup-a", "reset-due", "busy"}
	if len(got) != len(want) {
		t.Fatalf("candidates = %v", got)
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("candidates = %v, missing %s", got, id)
		}
	}
}

func TestScanStoresResultsAndMarksDeniedAccounts(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	probe := func(_ context.Context, id string) (int, []byte, error) {
		if id == "dead" {
			return http.StatusUnauthorized, nil, nil
		}
		return http.StatusOK, []byte(`{"plan_type":"free","rate_limit":{"primary_window":{"used_percent":7,"limit_window_seconds":2592000,"reset_after_seconds":100}}}`), nil
	}
	s := newTestService(t, &fakeSource{}, probe, &now)
	s.settings.ScanEnabled = true
	s.accounts = map[string]*Account{"ok": {ID: "ok", AccountID: "1"}, "dead": {ID: "dead", AccountID: "2"}}
	s.runScan(context.Background(), []string{"ok", "dead"}, 2)
	if account := s.accounts["ok"]; account.Plan != "free" || account.Source != sourceScan || len(account.Windows) != 1 {
		t.Fatalf("ok = %+v", account)
	}
	if !s.accounts["dead"].isInvalid() || s.scan.Succeeded != 1 || s.scan.Failed != 1 || s.scan.Running {
		t.Fatalf("dead=%+v scan=%+v", s.accounts["dead"], s.scan)
	}
}

func TestScanStopsWhenSwitchedOff(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	calls := 0
	var s *Service
	probe := func(context.Context, string) (int, []byte, error) {
		calls++
		s.mu.Lock()
		s.settings.ScanEnabled = false
		s.mu.Unlock()
		return http.StatusInternalServerError, nil, nil
	}
	s = newTestService(t, &fakeSource{}, probe, &now)
	s.settings.ScanEnabled = true
	s.accounts = map[string]*Account{"a": {ID: "a"}, "b": {ID: "b"}, "c": {ID: "c"}}
	s.runScan(context.Background(), []string{"a", "b", "c"}, 1)
	if calls > 2 {
		t.Fatalf("probes after switch off: %d", calls)
	}
}

func TestStatePersistsAcrossRestartWithoutJSONFiles(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	source := &fakeSource{observations: []coreauth.CodexQuotaObservation{{ID: "a", Name: "a", AccountID: "x", ObservedAt: now, Signals: signals(30, 43200, now.Add(time.Hour), "free")}}}
	first := New(dir, func() Source { return source }, nil)
	first.now = func() time.Time { return now }
	settings := DefaultSettings()
	settings.ScanEnabled = true
	settings.ScanConcurrency = 9
	if errUpdate := first.UpdateSettings(settings); errUpdate != nil {
		t.Fatalf("UpdateSettings() error = %v", errUpdate)
	}
	first.tick(context.Background())

	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		t.Fatalf("read dir: %v", errRead)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".json" {
			t.Fatalf("state file %s would be loaded as a credential", entry.Name())
		}
	}

	second := New(dir, func() Source { return &fakeSource{} }, nil)
	second.now = func() time.Time { return now }
	if got := second.Settings(); !got.ScanEnabled || got.ScanConcurrency != 9 {
		t.Fatalf("settings = %+v", got)
	}
	if account := second.accounts["a"]; account == nil || len(account.Windows) != 1 || account.Plan != "free" {
		t.Fatalf("account = %+v", account)
	}
	if len(second.history.samples) != 1 {
		t.Fatalf("history = %d samples", len(second.history.samples))
	}
}

func TestHistoryDropsExpiredAndPartialLines(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	path := filepath.Join(t.TempDir(), historyFile)
	for _, at := range []time.Time{now.Add(-31 * 24 * time.Hour), now.Add(-time.Hour)} {
		if errAppend := appendHistory(path, sample{Time: at}); errAppend != nil {
			t.Fatalf("appendHistory() error = %v", errAppend)
		}
	}
	file, errOpen := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if errOpen != nil {
		t.Fatalf("open: %v", errOpen)
	}
	_, _ = file.WriteString(`{"t":"2026`)
	_ = file.Close()
	loaded, errLoad := loadHistory(path, now)
	if errLoad != nil || len(loaded.samples) != 1 {
		t.Fatalf("samples=%d err=%v", len(loaded.samples), errLoad)
	}
}

func TestSettingsValidationAndSummaryJSONShape(t *testing.T) {
	bad := DefaultSettings()
	bad.ScanConcurrency = 0
	if bad.Validate() == nil {
		t.Fatal("expected invalid concurrency to fail")
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	data, errMarshal := json.Marshal(buildSummary(map[string]*Account{}, &history{}, capacityInput{FallbackLimit: 20}, DefaultSettings(), now))
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	var decoded map[string]any
	if errParse := json.Unmarshal(data, &decoded); errParse != nil {
		t.Fatalf("unmarshal: %v", errParse)
	}
	if plans, ok := decoded["plans"].([]any); !ok || len(plans) != 0 {
		t.Fatalf("plans = %v", decoded["plans"])
	}
}

func TestHomeModeReportsUnavailableWithoutScanning(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	calls := 0
	probe := func(context.Context, string) (int, []byte, error) { calls++; return 0, nil, nil }
	s := newTestService(t, &fakeSource{home: true}, probe, &now)
	s.settings.ScanEnabled = true
	s.tick(context.Background())
	if got := s.Summary(); got == nil || !got.HomeMode || got.Available {
		t.Fatalf("summary = %+v", got)
	}
	if calls != 0 {
		t.Fatalf("probes = %d", calls)
	}
}
