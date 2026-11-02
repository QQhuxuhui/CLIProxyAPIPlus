package codexspeed

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// Runtime holds the loaded hourly history and persists new records.
type Runtime struct {
	store   *Store
	nowFunc func() time.Time

	mu    sync.RWMutex
	hours []HourRecord
}

var defaultRuntime atomic.Pointer[Runtime]

// SetDefault installs the process-wide runtime read by the management API.
func SetDefault(r *Runtime) { defaultRuntime.Store(r) }

// Default returns the process-wide runtime, or nil before the service started it.
func Default() *Runtime { return defaultRuntime.Load() }

// NewRuntime loads the store's history.
func NewRuntime(store *Store, now func() time.Time) *Runtime {
	if now == nil {
		now = time.Now
	}
	r := &Runtime{store: store, nowFunc: now}
	r.hours = store.LoadHours(now())
	return r
}

// Hours returns the in-memory history, oldest first.
func (r *Runtime) Hours() []HourRecord {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.hours)
}

// Record appends rec to memory and disk. A disk failure is logged and the record kept in memory.
func (r *Runtime) Record(rec HourRecord) {
	if r == nil {
		return
	}
	now := r.nowFunc()
	r.mu.Lock()
	cutoff := now.Add(-Retention)
	kept := r.hours[:0]
	for _, h := range r.hours {
		if !h.Hour.Before(cutoff) {
			kept = append(kept, h)
		}
	}
	r.hours = append(kept, rec)
	r.mu.Unlock()
	if err := r.store.AppendHour(rec, now); err != nil {
		log.WithError(err).Error("codex speed: persist hourly record failed")
	}
}

// Store returns the runtime's store.
func (r *Runtime) Store() *Store {
	if r == nil {
		return nil
	}
	return r.store
}

// QuotaReading is one account's used percent on its longest quota window.
type QuotaReading struct {
	AccountKey  string
	UsedPercent float64
}

var quotaProvider atomic.Pointer[func() map[string]QuotaReading]

// SetQuotaProvider registers the source of per-auth quota readings (keyed by auth ID).
func SetQuotaProvider(fn func() map[string]QuotaReading) {
	if fn == nil {
		quotaProvider.Store(nil)
		return
	}
	quotaProvider.Store(&fn)
}

// QuotaReadings returns the current readings, or nil when no provider is registered.
func QuotaReadings() map[string]QuotaReading {
	if fn := quotaProvider.Load(); fn != nil {
		return (*fn)()
	}
	return nil
}
