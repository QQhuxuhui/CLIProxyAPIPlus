package usagestats

import (
	"context"
	"sync"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestAggregator_RecordAndQuery(t *testing.T) {
	dir := t.TempDir()
	a := newAggregator(dir)
	a.setEnabled(true)
	day := time.Date(2026, 7, 3, 10, 0, 0, 0, time.Local)
	rec := func(model string, failed bool) coreusage.Record {
		return coreusage.Record{AuthIndex: "1", Model: model, RequestedAt: day, Failed: failed}
	}
	a.HandleUsage(context.Background(), rec("gemini-3-pro", false))
	a.HandleUsage(context.Background(), rec("gemini-3-pro", false))
	a.HandleUsage(context.Background(), rec("gemini-3-pro", true))
	a.HandleUsage(context.Background(), rec("gemini-3-flash", false))

	stats, err := a.query(day, day, "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(stats) != 1 || stats[0].Account != "1" {
		t.Fatalf("stats = %+v, want one account '1'", stats)
	}
	var pro *ModelStat
	for i := range stats[0].Models {
		if stats[0].Models[i].Model == "gemini-3-pro" {
			pro = &stats[0].Models[i]
		}
	}
	if pro == nil || pro.Success != 2 || pro.Fail != 1 || pro.Total != 3 {
		t.Errorf("gemini-3-pro = %+v, want success 2 fail 1 total 3", pro)
	}
}

func TestAggregator_DisabledDropsRecords(t *testing.T) {
	a := newAggregator(t.TempDir())
	a.setEnabled(false)
	day := time.Date(2026, 7, 3, 10, 0, 0, 0, time.Local)
	a.HandleUsage(context.Background(), coreusage.Record{AuthIndex: "1", Model: "m", RequestedAt: day})
	stats, err := a.query(day, day, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 0 {
		t.Errorf("disabled aggregator must record nothing, got %+v", stats)
	}
}

func TestAggregator_QueryAccountFilterAndFlushReload(t *testing.T) {
	dir := t.TempDir()
	a := newAggregator(dir)
	a.setEnabled(true)
	day := time.Date(2026, 7, 3, 10, 0, 0, 0, time.Local)
	a.HandleUsage(context.Background(), coreusage.Record{AuthIndex: "1", Model: "m", RequestedAt: day})
	a.HandleUsage(context.Background(), coreusage.Record{AuthIndex: "2", Model: "m", RequestedAt: day})
	a.flush()
	// New aggregator over the same dir reads persisted data (restart simulation).
	b := newAggregator(dir)
	stats, err := b.query(day, day, "2")
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Account != "2" {
		t.Errorf("account filter = %+v, want only account '2'", stats)
	}
}

// memHasDay reports whether day is resident in a.mem (test helper).
func (a *aggregator) memHasDay(day string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.mem[day]
	return ok
}

// TestAggregator_EvictStaleMemDays asserts the in-memory day map is bounded to
// today+yesterday: an old, already-persisted day is evicted from a.mem yet its
// counts remain queryable from disk; a dirty (unsaved) old day is never
// evicted; today and yesterday stay resident. It fails if eviction is a no-op
// (old day would linger in a.mem forever).
func TestAggregator_EvictStaleMemDays(t *testing.T) {
	dir := t.TempDir()
	a := newAggregator(dir)
	a.setEnabled(true)

	now := time.Date(2026, 7, 3, 10, 0, 0, 0, time.Local)
	today := now.Format(dayLayout)
	yesterday := now.AddDate(0, 0, -1).Format(dayLayout)
	old := now.AddDate(0, 0, -5) // 5 days ago -> outside retention window
	oldDay := old.Format(dayLayout)

	rec := func(ts time.Time) coreusage.Record {
		return coreusage.Record{AuthIndex: "1", Model: "m", RequestedAt: ts, Failed: false}
	}
	a.HandleUsage(context.Background(), rec(old))
	a.HandleUsage(context.Background(), rec(now.AddDate(0, 0, -1)))
	a.HandleUsage(context.Background(), rec(now))

	// Persist so the old day can be safely evicted.
	a.flush()
	a.evictStaleMemDays(now)

	if a.memHasDay(oldDay) {
		t.Fatalf("old day %s still resident in a.mem after eviction", oldDay)
	}
	if !a.memHasDay(today) {
		t.Fatalf("today %s must remain resident", today)
	}
	if !a.memHasDay(yesterday) {
		t.Fatalf("yesterday %s must remain resident", yesterday)
	}

	// Evicted day is transparently reloaded from disk on query.
	stats, err := a.query(old, old, "1")
	if err != nil {
		t.Fatalf("query old day: %v", err)
	}
	if len(stats) != 1 || len(stats[0].Models) != 1 || stats[0].Models[0].Success != 1 {
		t.Fatalf("evicted old day lost counts on disk-reload: %+v", stats)
	}

	// A dirty (unsaved) old day must not be evicted.
	dirtyOld := now.AddDate(0, 0, -6)
	dirtyOldDay := dirtyOld.Format(dayLayout)
	a.HandleUsage(context.Background(), rec(dirtyOld)) // marks day dirty, not flushed
	a.evictStaleMemDays(now)
	if !a.memHasDay(dirtyOldDay) {
		t.Fatalf("dirty old day %s must not be evicted before it is persisted", dirtyOldDay)
	}
}

// TestAggregator_ConcurrentRecordAndQuery drives HandleUsage and query
// concurrently so the -race run actually exercises the mem-map lock. Without
// real concurrency the race detector finds nothing — a placebo test.
func TestAggregator_ConcurrentRecordAndQuery(t *testing.T) {
	dir := t.TempDir()
	a := newAggregator(dir)
	a.setEnabled(true)
	day := time.Date(2026, 7, 3, 10, 0, 0, 0, time.Local)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			a.HandleUsage(context.Background(), coreusage.Record{AuthIndex: "1", Model: "m", RequestedAt: day})
		}()
		go func() {
			defer wg.Done()
			_, _ = a.query(day, day, "")
		}()
	}
	wg.Wait()
	stats, err := a.query(day, day, "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || len(stats[0].Models) != 1 || stats[0].Models[0].Success != 50 {
		t.Errorf("concurrent counts lost updates: want 50 success, got %+v", stats)
	}
}

// TestAggregator_ConcurrentRecordAndFlush drives HandleUsage and flush
// concurrently. In production flushLoop ticks in the background while every
// request calls HandleUsage, so flush must not read the live in-memory
// dayData without holding the same lock HandleUsage writes under.
func TestAggregator_ConcurrentRecordAndFlush(t *testing.T) {
	dir := t.TempDir()
	a := newAggregator(dir)
	a.setEnabled(true)
	day := time.Date(2026, 7, 3, 10, 0, 0, 0, time.Local)

	// A single background goroutine hammers flush while the record
	// goroutines run, mirroring flushLoop ticking during live traffic.
	stop := make(chan struct{})
	var flushWG sync.WaitGroup
	flushWG.Add(1)
	go func() {
		defer flushWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				a.flush()
			}
		}
	}()

	var recordWG sync.WaitGroup
	for i := 0; i < 50; i++ {
		recordWG.Add(1)
		go func() {
			defer recordWG.Done()
			a.HandleUsage(context.Background(), coreusage.Record{AuthIndex: "1", Model: "m", RequestedAt: day})
		}()
	}
	recordWG.Wait()
	close(stop)
	flushWG.Wait()
	a.flush() // final flush to persist anything left dirty

	// Reload from disk on a fresh aggregator to confirm no updates were lost.
	b := newAggregator(dir)
	stats, err := b.query(day, day, "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || len(stats[0].Models) != 1 || stats[0].Models[0].Success != 50 {
		t.Errorf("concurrent flush lost updates: want 50 success, got %+v", stats)
	}
}
