package helps

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func codexFastSessionTestPool(percent int, members int) *CodexFastSessionPool {
	seed := make([]string, 0, members)
	for i := 0; i < members; i++ {
		seed = append(seed, fmt.Sprintf("fast-%02d", i))
	}
	pool := NewCodexFastSessionPool()
	pool.ApplyConfig(config.CodexFastSessionIDsConfig{Enable: true, Percent: percent, Seed: seed})
	return pool
}

func codexFastSessionTestIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("conversation-%d", i)
	}
	return ids
}

func selectAll(t *testing.T, pool *CodexFastSessionPool, ids []string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		value, group := pool.Select(id)
		if group != CodexFastSessionGroupFast {
			t.Fatalf("Select(%q) group = %q, want fast", id, group)
		}
		out[id] = value
	}
	return out
}

func TestCodexFastSessionRendezvousRemoveRemapsOnlyItsIdentities(t *testing.T) {
	pool := codexFastSessionTestPool(100, 12)
	ids := codexFastSessionTestIDs(5000)
	before := selectAll(t, pool, ids)

	removed := "fast-05"
	if !pool.Remove(removed) {
		t.Fatal("Remove returned false")
	}
	after := selectAll(t, pool, ids)
	for _, id := range ids {
		if before[id] != removed && after[id] != before[id] {
			t.Fatalf("identity %q moved from %q to %q although %q was removed", id, before[id], after[id], removed)
		}
		if after[id] == removed {
			t.Fatalf("identity %q still maps to removed member", id)
		}
	}
}

func TestCodexFastSessionRendezvousAddRemapsAboutOneNth(t *testing.T) {
	pool := codexFastSessionTestPool(100, 12)
	ids := codexFastSessionTestIDs(5000)
	before := selectAll(t, pool, ids)

	if !pool.Add("fast-new") {
		t.Fatal("Add returned false")
	}
	after := selectAll(t, pool, ids)
	moved := 0
	for _, id := range ids {
		if after[id] != before[id] {
			moved++
			if after[id] != "fast-new" {
				t.Fatalf("identity %q moved to %q, want only moves onto the new member", id, after[id])
			}
		}
	}
	share := float64(moved) / float64(len(ids))
	if share <= 0 || share >= 0.20 {
		t.Fatalf("moved share = %.3f, want (0, 0.20) (about 1/13)", share)
	}
}

func TestCodexFastSessionCanaryDeterministicAndShare(t *testing.T) {
	pool := codexFastSessionTestPool(10, 12)
	ids := codexFastSessionTestIDs(20000)
	fast := 0
	for _, id := range ids {
		value, group := pool.Select(id)
		value2, group2 := pool.Select(id)
		if value != value2 || group != group2 {
			t.Fatalf("Select(%q) not deterministic: %q/%q vs %q/%q", id, value, group, value2, group2)
		}
		switch group {
		case CodexFastSessionGroupFast:
			fast++
		case CodexFastSessionGroupControl:
			if value != id {
				t.Fatalf("control group must keep the original value, got %q", value)
			}
		default:
			t.Fatalf("unexpected group %q", group)
		}
	}
	share := float64(fast) / float64(len(ids))
	if share < 0.08 || share > 0.12 {
		t.Fatalf("fast share = %.3f, want about 0.10", share)
	}

	// The canary gate must not depend on pool membership.
	pool.Add("fast-extra")
	for _, id := range ids[:2000] {
		_, before := codexFastSessionTestPool(10, 12).Select(id)
		_, after := pool.Select(id)
		if before != after {
			t.Fatalf("canary group of %q changed after pool change: %q -> %q", id, before, after)
		}
	}
}

func TestCodexFastSessionPassthrough(t *testing.T) {
	disabled := NewCodexFastSessionPool()
	disabled.ApplyConfig(config.CodexFastSessionIDsConfig{Enable: false, Percent: 100, Seed: []string{"fast-a"}})
	empty := NewCodexFastSessionPool()
	empty.ApplyConfig(config.CodexFastSessionIDsConfig{Enable: true, Percent: 100})
	enabled := codexFastSessionTestPool(100, 3)

	cases := []struct {
		name string
		pool *CodexFastSessionPool
		id   string
	}{
		{"disabled", disabled, "conv"},
		{"empty pool", empty, "conv"},
		{"empty id", enabled, ""},
		{"nil pool", nil, "conv"},
	}
	for _, tc := range cases {
		value, group := tc.pool.Select(tc.id)
		if value != tc.id || group != CodexFastSessionGroupOff {
			t.Fatalf("%s: Select = %q/%q, want %q/off", tc.name, value, group, tc.id)
		}
	}
	if value, group, _ := SelectCodexFastSession(nil, "conv"); value != "conv" || group != CodexFastSessionGroupOff {
		t.Fatalf("nil config: Select = %q/%q, want conv/off", value, group)
	}
}

func TestCodexFastSessionApplyConfigReloadsOnlyOnSeedChange(t *testing.T) {
	pool := NewCodexFastSessionPool()
	pool.ApplyConfig(config.CodexFastSessionIDsConfig{Enable: true, Percent: 10, Seed: []string{" a ", "b", "a", ""}})
	if got := pool.Members(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("members = %v, want [a b]", got)
	}

	// Runtime edits survive a reload with the same seed (e.g. a percent change).
	pool.Add("c")
	pool.Remove("a")
	pool.ApplyConfig(config.CodexFastSessionIDsConfig{Enable: true, Percent: 50, Seed: []string{"a", "b"}})
	if got := pool.Members(); !slices.Equal(got, []string{"b", "c"}) {
		t.Fatalf("members after same-seed reload = %v, want [b c]", got)
	}
	if snap := pool.Snapshot(10); snap.Percent != 50 || !snap.Enable {
		t.Fatalf("snapshot enable/percent = %t/%d, want true/50", snap.Enable, snap.Percent)
	}

	// A seed change replaces the members.
	pool.ApplyConfig(config.CodexFastSessionIDsConfig{Enable: true, Percent: 50, Seed: []string{"x"}})
	if got := pool.Members(); !slices.Equal(got, []string{"x"}) {
		t.Fatalf("members after seed change = %v, want [x]", got)
	}

	pool.Replace([]string{"y", "y", " z "})
	if got := pool.Members(); !slices.Equal(got, []string{"y", "z"}) {
		t.Fatalf("members after Replace = %v, want [y z]", got)
	}

	// Percent is clamped.
	pool.ApplyConfig(config.CodexFastSessionIDsConfig{Enable: true, Percent: 150, Seed: []string{"x"}})
	if snap := pool.Snapshot(10); snap.Percent != 100 {
		t.Fatalf("percent = %d, want 100", snap.Percent)
	}
}

func TestCodexFastSessionObserveStats(t *testing.T) {
	pool := NewCodexFastSessionPool()
	now := time.Date(2026, 10, 25, 18, 0, 30, 0, time.UTC)
	pool.nowFunc = func() time.Time { return now }
	for i := 1; i <= 10; i++ {
		pool.Observe(CodexFastSessionGroupFast, false, time.Duration(i)*100*time.Millisecond, 100)
	}
	pool.Observe(CodexFastSessionGroupFast, false, 9*time.Second, 50000) // large request: no ttft sample
	pool.Observe(CodexFastSessionGroupFast, true, 0, 100)
	pool.Observe(CodexFastSessionGroupControl, false, 5*time.Second, 100)
	pool.Observe(CodexFastSessionGroupOff, false, time.Second, 100)

	snap := pool.Snapshot(10)
	fast := snap.Groups[CodexFastSessionGroupFast]
	if fast.TotalRequests != 12 || fast.TotalFailures != 1 || fast.WindowRequests != 12 || fast.WindowFailures != 1 || fast.SmallTTFTSamples != 10 {
		t.Fatalf("fast stats = %+v", fast)
	}
	if fast.SmallTTFTP50Ms != 500 || fast.SmallTTFTP90Ms != 900 {
		t.Fatalf("fast p50/p90 = %d/%d, want 500/900", fast.SmallTTFTP50Ms, fast.SmallTTFTP90Ms)
	}
	control := snap.Groups[CodexFastSessionGroupControl]
	if control.TotalRequests != 1 || control.SmallTTFTP50Ms != 5000 {
		t.Fatalf("control stats = %+v", control)
	}
	if _, ok := snap.Groups[CodexFastSessionGroupOff]; ok {
		t.Fatal("off group must not be tracked")
	}

	// Samples age out of the window but stay in the totals.
	now = now.Add(11 * time.Minute)
	fast = pool.Snapshot(10).Groups[CodexFastSessionGroupFast]
	if fast.TotalRequests != 12 || fast.WindowRequests != 0 || fast.SmallTTFTSamples != 0 {
		t.Fatalf("aged fast stats = %+v", fast)
	}
}

func TestCodexFastSessionObserveRecordFilters(t *testing.T) {
	before := CodexFastSessions().Snapshot(10).Groups[CodexFastSessionGroupControl].TotalRequests
	observeCodexFastSessionRecord(CodexFastSessionGroupControl, usage.Record{AdditionalModel: true})
	observeCodexFastSessionRecord(CodexFastSessionGroupControl, usage.Record{Generate: usage.GenerateFlag(false)})
	observeCodexFastSessionRecord(CodexFastSessionGroupControl, usage.Record{TTFT: time.Second})
	after := CodexFastSessions().Snapshot(10).Groups[CodexFastSessionGroupControl].TotalRequests
	if after != before+1 {
		t.Fatalf("control requests = %d, want %d", after, before+1)
	}
}

func TestCodexFastSessionFullRolloutKeepsRollbackControl(t *testing.T) {
	pool := NewCodexFastSessionPool()
	cfg := config.CodexFastSessionIDsConfig{Enable: true, Percent: 100, Seed: []string{"fast"}, Rollback: config.CodexFastSessionRollbackConfig{Enable: true}}
	cfg.Normalize()
	pool.ApplyConfig(cfg)
	for _, id := range codexFastSessionTestIDs(20000) {
		_, group := pool.Select(id)
		pool.Observe(group, group == CodexFastSessionGroupFast, time.Second, 100)
	}
	pool.EvaluateRollback(cfg.Rollback)
	snap := pool.Snapshot(10)
	if !snap.Rollback.Disabled {
		t.Fatalf("all fast requests failed without rollback: %+v, control=%d", snap.RollbackCheck, snap.Groups[CodexFastSessionGroupControl].WindowRequests)
	}
	cfg.Rollback.Enable = false
	pool.ApplyConfig(cfg)
	for _, id := range codexFastSessionTestIDs(1000) {
		if _, group := pool.Select(id); group != CodexFastSessionGroupFast {
			t.Fatalf("rollback disabled: %s selected %s", id, group)
		}
	}
}

func TestCodexFastSessionReenableClearsEvaluationWindow(t *testing.T) {
	pool, cfg := codexFastSessionRollbackPool(t)
	observeN(pool, CodexFastSessionGroupFast, 30, 30, 0)
	observeN(pool, CodexFastSessionGroupControl, 30, 0, time.Second)
	pool.EvaluateRollback(cfg.Rollback)
	cfg.Seed = []string{"replacement"}
	pool.ApplyConfig(cfg)
	pool.EvaluateRollback(cfg.Rollback)
	snap := pool.Snapshot(10)
	if snap.Rollback.Disabled || snap.Groups[CodexFastSessionGroupFast].WindowRequests != 0 {
		t.Fatalf("old samples disabled fresh pool: %+v", snap)
	}
	if snap.Groups[CodexFastSessionGroupFast].TotalRequests != 30 {
		t.Fatal("lifetime totals should survive a new evaluation window")
	}
}

func TestCodexFastSessionOldInflightDoesNotRefillWindow(t *testing.T) {
	pool := NewCodexFastSessionPool()
	cfg := config.CodexFastSessionIDsConfig{Enable: true, Percent: 100, Seed: []string{"old"}}
	pool.ApplyConfig(cfg)
	_, group, epoch := pool.selectTracked("conversation")
	cfg.Seed = []string{"new"}
	pool.ApplyConfig(cfg)
	pool.observeGeneration(group, true, 0, 100, epoch)
	_, group, epoch = pool.selectTracked("conversation")
	pool.observeGeneration(group, false, time.Second, 100, epoch)
	stats := pool.Snapshot(10).Groups[CodexFastSessionGroupFast]
	if stats.WindowRequests != 1 || stats.WindowFailures != 0 || stats.TotalRequests != 2 || stats.TotalFailures != 1 {
		t.Fatalf("mixed configuration generations: %+v", stats)
	}
}

func TestCodexFastSessionReporterRetainsSelectionGeneration(t *testing.T) {
	pool := CodexFastSessions()
	cfg := config.CodexFastSessionIDsConfig{Enable: true, Percent: 100, Seed: []string{"old-inflight"}}
	pool.ApplyConfig(cfg)
	_, group, epoch := pool.selectTracked("conversation")
	reporter := &UsageReporter{generate: true}
	reporter.SetCodexFastSessionGroup(group, epoch)
	cfg.Seed = []string{"new-inflight"}
	pool.ApplyConfig(cfg)
	reporter.PublishFailure(context.Background(), fmt.Errorf("late failure"))
	if stats := pool.Snapshot(10).Groups[CodexFastSessionGroupFast]; stats.WindowRequests != 0 {
		t.Fatalf("old reporter contaminated new window: %+v", stats)
	}
}

func TestCodexFastSessionConcurrentReloadAndObservation(t *testing.T) {
	pool := NewCodexFastSessionPool()
	cfg := config.CodexFastSessionIDsConfig{Enable: true, Percent: 50, Seed: []string{"fast"}, Rollback: config.CodexFastSessionRollbackConfig{Enable: true}}
	cfg.Normalize()
	pool.ApplyConfig(cfg)
	var workers sync.WaitGroup
	for worker := 0; worker < 3; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := 0; i < 100; i++ {
				_, group, epoch := pool.selectTracked(fmt.Sprint(i))
				pool.observeGeneration(group, false, time.Second, 100, epoch)
				pool.Snapshot(10)
				pool.EvaluateRollback(cfg.Rollback)
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		changed := cfg
		for i := 0; i < 100; i++ {
			changed.Percent = 50 + i%2
			pool.ApplyConfig(changed)
		}
	}()
	workers.Wait()
	snap := pool.Snapshot(10)
	if snap.Groups[CodexFastSessionGroupFast].TotalRequests+snap.Groups[CodexFastSessionGroupControl].TotalRequests != 300 {
		t.Fatal("concurrent reload lost lifetime observations")
	}
}
