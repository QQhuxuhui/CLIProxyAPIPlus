package usagestats

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const dayLayout = "2006-01-02"

// inMemRetentionDays bounds how many recent calendar days stay resident in
// a.mem (today + yesterday). Without eviction a.mem gains one entry per day of
// process uptime and never shrinks. query() transparently reloads older days
// from disk, so evicting them is safe.
const inMemRetentionDays = 2

// ModelStat is one model's counts in a query result.
type ModelStat struct {
	Model   string `json:"model"`
	Success int64  `json:"success"`
	Fail    int64  `json:"fail"`
	Total   int64  `json:"total"`
}

// AccountStat groups a single account's per-model counts.
type AccountStat struct {
	Account string      `json:"account"`
	Models  []ModelStat `json:"models"`
}

type aggregator struct {
	store   *store
	mu      sync.Mutex
	mem     map[string]*dayData // day -> data
	dirty   map[string]bool
	enabled atomic.Bool
}

func newAggregator(dir string) *aggregator {
	return &aggregator{
		store: newStore(dir),
		mem:   make(map[string]*dayData),
		dirty: make(map[string]bool),
	}
}

func (a *aggregator) setEnabled(v bool) { a.enabled.Store(v) }

// HandleUsage implements coreusage.Plugin.
func (a *aggregator) HandleUsage(ctx context.Context, rec coreusage.Record) {
	if a == nil || !a.enabled.Load() {
		return
	}
	account := rec.AuthIndex
	if account == "" {
		return
	}
	model := rec.Model
	if model == "" {
		model = "unknown"
	}
	ts := rec.RequestedAt
	if ts.IsZero() {
		ts = time.Now()
	}
	day := ts.Local().Format(dayLayout)

	a.mu.Lock()
	defer a.mu.Unlock()
	d := a.mem[day]
	if d == nil {
		loaded, err := a.store.load(day)
		if err != nil {
			log.Warnf("usagestats: load day %s: %v", day, err)
			loaded = newDayData(day)
		}
		d = loaded
		a.mem[day] = d
	}
	models := d.Accounts[account]
	if models == nil {
		models = make(map[string]*Counts)
		d.Accounts[account] = models
	}
	c := models[model]
	if c == nil {
		c = &Counts{}
		models[model] = c
	}
	if rec.Failed {
		c.Fail++
	} else {
		c.Success++
	}
	a.dirty[day] = true
}

// copyDayData deep-copies a dayData so it can be serialized without holding
// a.mu: HandleUsage mutates the live instance (map entries and Counts
// fields) under a.mu, so flush must snapshot under the same lock rather
// than share the pointer with a background disk write.
func copyDayData(d *dayData) *dayData {
	cp := newDayData(d.Date)
	for acc, models := range d.Accounts {
		mcopy := make(map[string]*Counts, len(models))
		for model, c := range models {
			cc := *c
			mcopy[model] = &cc
		}
		cp.Accounts[acc] = mcopy
	}
	return cp
}

// flush persists all dirty days.
func (a *aggregator) flush() {
	a.mu.Lock()
	pending := make(map[string]*dayData, len(a.dirty))
	for day := range a.dirty {
		if d := a.mem[day]; d != nil {
			pending[day] = copyDayData(d)
		}
	}
	a.dirty = make(map[string]bool)
	a.mu.Unlock()
	for day, d := range pending {
		if err := a.store.save(day, d); err != nil {
			log.Warnf("usagestats: flush day %s: %v", day, err)
			a.mu.Lock()
			a.dirty[day] = true // retry next flush
			a.mu.Unlock()
		}
	}
}

// evictStaleMemDays drops from a.mem any resident day older than the
// in-memory retention window (today plus inMemRetentionDays-1 prior days). A
// day with unsaved counts (a.dirty) is kept so it is not lost before the next
// flush; days whose key fails to parse are left in place defensively. Evicted
// days are transparently reloaded from disk by query(), so correctness holds.
// Call this only after flush() so a day is persisted before it can be evicted.
func (a *aggregator) evictStaleMemDays(now time.Time) {
	cutoff := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).
		AddDate(0, 0, -(inMemRetentionDays - 1))
	a.mu.Lock()
	defer a.mu.Unlock()
	for day := range a.mem {
		if a.dirty[day] {
			continue
		}
		t, err := time.ParseInLocation(dayLayout, day, now.Location())
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			delete(a.mem, day)
		}
	}
}

// mergeDayInto adds one day's counts into agg, optionally filtered to a
// single account. Callers must ensure data is not concurrently mutated
// while this runs (either by holding a.mu, or by owning a private copy).
func mergeDayInto(agg map[string]map[string]*Counts, data *dayData, account string) {
	for acc, models := range data.Accounts {
		if account != "" && acc != account {
			continue
		}
		dst := agg[acc]
		if dst == nil {
			dst = make(map[string]*Counts)
			agg[acc] = dst
		}
		for model, c := range models {
			cur := dst[model]
			if cur == nil {
				cur = &Counts{}
				dst[model] = cur
			}
			cur.Success += c.Success
			cur.Fail += c.Fail
		}
	}
}

// query ranges inclusively over [from, to] days and sums per (account, model).
func (a *aggregator) query(from, to time.Time, account string) ([]AccountStat, error) {
	from = from.Local()
	to = to.Local()
	// account -> model -> counts
	agg := make(map[string]map[string]*Counts)
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		day := d.Format(dayLayout)
		// Merge in-memory day data while still holding a.mu: HandleUsage
		// mutates the same dayData/Counts under this lock, so reading it
		// must happen under the lock too (not just the pointer fetch).
		a.mu.Lock()
		mem, inMem := a.mem[day]
		if inMem {
			mergeDayInto(agg, mem, account)
		}
		a.mu.Unlock()
		if inMem {
			continue
		}
		// Not resident in memory: load a private copy from disk, which no
		// other goroutine can be mutating, so it's safe to merge unlocked.
		loaded, err := a.store.load(day)
		if err != nil {
			return nil, err
		}
		mergeDayInto(agg, loaded, account)
	}
	out := make([]AccountStat, 0, len(agg))
	for acc, models := range agg {
		ms := make([]ModelStat, 0, len(models))
		for model, c := range models {
			ms = append(ms, ModelStat{Model: model, Success: c.Success, Fail: c.Fail, Total: c.Success + c.Fail})
		}
		sort.Slice(ms, func(i, j int) bool { return ms[i].Model < ms[j].Model })
		out = append(out, AccountStat{Account: acc, Models: ms})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out, nil
}

// --- package-level singleton wiring (used by service + handler) ---

var (
	global     *aggregator
	globalOnce sync.Once
	retention  atomic.Int64
)

// Init builds the global aggregator over dir, starts its flush loop, and
// registers it on the coreusage bus. Idempotent.
func Init(dir string) {
	globalOnce.Do(func() {
		global = newAggregator(dir)
		retention.Store(90)
		coreusage.RegisterPlugin(global)
		go global.flushLoop()
	})
}

func (a *aggregator) flushLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		a.flush()
		a.store.sweep(time.Now(), int(retention.Load()))
		// Evict after flush so a day is persisted before it leaves memory.
		a.evictStaleMemDays(time.Now())
	}
}

// SetEnabled toggles recording on the global aggregator.
func SetEnabled(v bool) {
	if global != nil {
		global.setEnabled(v)
	}
}

// SetRetentionDays sets the day-file retention for sweeps.
func SetRetentionDays(d int) {
	if d > 0 {
		retention.Store(int64(d))
	}
}

// Flush persists dirty days immediately (shutdown hook).
func Flush() {
	if global != nil {
		global.flush()
	}
}

// Query ranges over persisted+in-memory counts. Returns empty if uninitialized.
func Query(from, to time.Time, account string) ([]AccountStat, error) {
	if global == nil {
		return []AccountStat{}, nil
	}
	return global.query(from, to, account)
}
