package codexquota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// DirName holds service state inside the auth dir. Its files deliberately do
	// not end in .json, so credential loaders never read them.
	DirName = ".codex-quota"

	settingsFile = "settings.conf"
	accountsFile = "accounts.dat"
	historyFile  = "history.jsonl"

	tickInterval     = time.Minute
	invalidRetry     = 6 * time.Hour
	historyCompactAt = 24 * time.Hour
	removalGrace     = 10 * time.Minute

	sourcePassive = "passive"
	sourceScan    = "scan"
)

// Settings are runtime options kept outside config.yaml so a change never
// triggers a configuration reload.
type Settings struct {
	ScanEnabled     bool    `json:"scan_enabled"`
	ScanConcurrency int     `json:"scan_concurrency"`
	StaleMinutes    int     `json:"stale_minutes"`
	HeadroomPercent float64 `json:"headroom_percent"`
	HorizonHours    int     `json:"horizon_hours"`
}

// DefaultSettings returns the initial options; scanning starts disabled.
func DefaultSettings() Settings {
	return Settings{ScanConcurrency: 5, StaleMinutes: 30, HeadroomPercent: 20, HorizonHours: 24}
}

// Validate rejects values outside the supported ranges.
func (s Settings) Validate() error {
	switch {
	case s.ScanConcurrency < 1 || s.ScanConcurrency > 50:
		return errors.New("scan_concurrency must be between 1 and 50")
	case s.StaleMinutes < 5 || s.StaleMinutes > 1440:
		return errors.New("stale_minutes must be between 5 and 1440")
	case s.HeadroomPercent < 0 || s.HeadroomPercent > 200:
		return errors.New("headroom_percent must be between 0 and 200")
	case s.HorizonHours < 1 || s.HorizonHours > maxForecastHours:
		return errors.New("horizon_hours must be between 1 and 720")
	}
	return nil
}

// ScanStatus reports backfill progress.
type ScanStatus struct {
	Running    bool      `json:"running"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Queued     int       `json:"queued"`
	Succeeded  int       `json:"succeeded"`
	Failed     int       `json:"failed"`
	LastError  string    `json:"last_error,omitempty"`
}

// Account is the cached quota view of one credential file.
type Account struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	AccountID  string    `json:"account_id,omitempty"`
	Plan       string    `json:"plan,omitempty"`
	Windows    []Window  `json:"windows,omitempty"`
	ObservedAt time.Time `json:"observed_at,omitempty"`
	Source     string    `json:"source,omitempty"`
	Disabled   bool      `json:"disabled,omitempty"`
	ScanAt     time.Time `json:"scan_at,omitempty"`
	ScanDenied bool      `json:"scan_denied,omitempty"`

	credentialInvalid bool
}

func (a *Account) key() string {
	if a.AccountID != "" {
		return a.AccountID
	}
	return "id:" + a.ID
}

func (a *Account) isInvalid() bool { return a.credentialInvalid || a.ScanDenied }

// Source supplies live auth state. Implemented by *coreauth.Manager.
type Source interface {
	CodexQuotaObservations() []coreauth.CodexQuotaObservation
	CodexPoolSnapshot() coreauth.CodexPoolSnapshot
	HomeEnabled() bool
}

// ProbeFunc fetches /wham/usage for one credential.
type ProbeFunc func(ctx context.Context, authID string) (status int, body []byte, err error)

// Service aggregates passive observations, optional scans and pool history.
type Service struct {
	dir    string
	source func() Source
	probe  ProbeFunc
	now    func() time.Time

	mu            sync.Mutex
	settings      Settings
	accounts      map[string]*Account
	last          map[string]Window
	history       *history
	burn          map[string]float64
	peakRPM       int
	lastSampleAt  time.Time
	lastCompactAt time.Time
	scan          ScanStatus
	startedAt     time.Time

	summary  atomic.Pointer[Summary]
	rows     atomic.Pointer[map[string]AccountRow]
	scanBusy atomic.Bool
	wake     chan struct{}
}

// New loads persisted state from dir and tolerates missing or damaged files.
func New(dir string, source func() Source, probe ProbeFunc) *Service {
	s := &Service{
		dir:      dir,
		source:   source,
		probe:    probe,
		now:      time.Now,
		settings: DefaultSettings(),
		accounts: map[string]*Account{},
		last:     map[string]Window{},
		history:  &history{},
		burn:     map[string]float64{},
		wake:     make(chan struct{}, 1),
	}
	s.startedAt = s.now()
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		log.WithError(errMkdir).Warn("codex quota: create state directory failed")
	}
	s.load()
	return s
}

func (s *Service) load() {
	if data, errRead := os.ReadFile(filepath.Join(s.dir, settingsFile)); errRead == nil {
		loaded := DefaultSettings()
		if errParse := json.Unmarshal(data, &loaded); errParse == nil && loaded.Validate() == nil {
			s.settings = loaded
		} else {
			log.Warn("codex quota: ignoring invalid settings file")
		}
	}
	if data, errRead := os.ReadFile(filepath.Join(s.dir, accountsFile)); errRead == nil {
		var cached []*Account
		if errParse := json.Unmarshal(data, &cached); errParse != nil {
			log.WithError(errParse).Warn("codex quota: ignoring damaged account cache")
		}
		for _, account := range cached {
			if account == nil || account.ID == "" {
				continue
			}
			s.accounts[account.ID] = account
			s.rememberWindows(account)
		}
	}
	loaded, errHistory := loadHistory(filepath.Join(s.dir, historyFile), s.now())
	if errHistory != nil {
		log.WithError(errHistory).Warn("codex quota: history partially loaded")
	}
	s.history = loaded
}

func (s *Service) rememberWindows(account *Account) {
	for _, window := range account.Windows {
		s.last[burnKey(account.key(), window.Minutes)] = window
	}
}

// Start runs the aggregation loop until ctx ends.
func (s *Service) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(tickInterval)
		defer ticker.Stop()
		s.tick(ctx)
		for {
			select {
			case <-ctx.Done():
				s.mu.Lock()
				s.saveAccountsLocked()
				s.mu.Unlock()
				return
			case <-ticker.C:
				s.tick(ctx)
			case <-s.wake:
				s.refresh(s.now())
				s.startScan(ctx)
			}
		}
	}()
}

func (s *Service) tick(ctx context.Context) {
	source := s.source()
	if source == nil {
		return
	}
	now := s.now()
	if source.HomeEnabled() {
		s.summary.Store(&Summary{HomeMode: true, GeneratedAt: now, Plans: []PlanStat{}, Trend: []TrendPoint{}, Settings: s.Settings()})
		return
	}
	s.merge(source.CodexQuotaObservations())
	s.record(source.CodexPoolSnapshot(), now)
	s.refresh(now)
	s.startScan(ctx)
}

// merge folds the latest passive observations into the cache.
func (s *Service) merge(observations []coreauth.CodexQuotaObservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	live := make(map[string]struct{}, len(observations))
	for _, item := range observations {
		live[item.ID] = struct{}{}
		account := s.accounts[item.ID]
		if account == nil {
			account = &Account{ID: item.ID}
			s.accounts[item.ID] = account
		}
		account.Name = item.Name
		account.AccountID = strings.TrimSpace(item.AccountID)
		account.Disabled = item.Disabled
		account.credentialInvalid = item.Invalid
		if plan := strings.ToLower(strings.TrimSpace(item.Plan)); plan != "" && account.Plan == "" {
			account.Plan = plan
		}
		if account.Plan == "" && item.IDToken != "" {
			if claims, errParse := codexauth.ParseJWTToken(item.IDToken); errParse == nil && claims != nil {
				account.Plan = strings.ToLower(strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType))
			}
		}
		if !item.ObservedAt.After(account.ObservedAt) {
			continue
		}
		windows, plan := WindowsFromSignals(item.Signals, item.ObservedAt)
		if len(windows) == 0 {
			continue
		}
		s.applyLocked(account, windows, plan, item.ObservedAt, sourcePassive)
	}
	// The manager may still be loading after a restart, so a missing account only
	// means removal once startup is well behind us.
	if s.now().Sub(s.startedAt) < removalGrace {
		return
	}
	for id := range s.accounts {
		if _, ok := live[id]; !ok {
			delete(s.accounts, id)
		}
	}
}

// applyLocked replaces windows and credits consumption since the last observation.
func (s *Service) applyLocked(account *Account, windows []Window, plan string, observedAt time.Time, source string) {
	if plan != "" {
		account.Plan = plan
	}
	group := account.Plan
	if group == "" {
		group = "unknown"
	}
	for _, window := range windows {
		key := burnKey(account.key(), window.Minutes)
		previous, seen := s.last[key]
		if !seen {
			s.last[key] = window
			continue
		}
		delta := window.UsedPercent
		// Reset times drift by seconds between responses, so only a larger jump is a new window.
		if !window.ResetAt.After(previous.ResetAt.Add(time.Minute)) {
			delta = window.UsedPercent - previous.UsedPercent
		}
		// Files sharing an account report out of order; only forward progress counts.
		if delta <= 0 {
			continue
		}
		s.last[key] = window
		s.burn[burnKey(group, window.Minutes)] += delta
	}
	account.Windows = windows
	account.ObservedAt = observedAt
	account.Source = source
	account.ScanDenied = false
}

func (s *Service) record(snapshot coreauth.CodexPoolSnapshot, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peakRPM = max(s.peakRPM, snapshot.RPM)
	if !s.lastSampleAt.IsZero() && now.Sub(s.lastSampleAt) < sampleInterval {
		return
	}
	item := sample{Time: now, PeakRPM: s.peakRPM, Burn: s.burn}
	s.fillSampleLocked(&item, snapshot, now)
	s.history.add(item, now)
	s.lastSampleAt = now
	s.peakRPM = 0
	s.burn = map[string]float64{}
	if errAppend := appendHistory(filepath.Join(s.dir, historyFile), item); errAppend != nil {
		log.WithError(errAppend).Warn("codex quota: append history failed")
	}
	if now.Sub(s.lastCompactAt) >= historyCompactAt {
		s.lastCompactAt = now
		if errRewrite := rewriteHistory(filepath.Join(s.dir, historyFile), s.history.samples); errRewrite != nil {
			log.WithError(errRewrite).Warn("codex quota: compact history failed")
		}
	}
	s.saveAccountsLocked()
}

func (s *Service) fillSampleLocked(item *sample, snapshot coreauth.CodexPoolSnapshot, now time.Time) {
	limit := fallbackLimit(snapshot)
	if snapshot.Schedulable > 0 && snapshot.Capacity > 0 {
		limit = snapshot.Capacity / snapshot.Schedulable
	}
	known, total := 0, 0.0
	for _, account := range uniqueAccounts(s.accounts) {
		if len(account.Windows) == 0 {
			continue
		}
		remaining, _ := remainingOf(account.Windows, now)
		known++
		total += remaining
		if remaining >= usableFloorPercent {
			item.Usable++
		}
	}
	item.Capacity = item.Usable * limit
	if known > 0 {
		item.AvgRemaining = round1(total / float64(known))
	}
}

func fallbackLimit(snapshot coreauth.CodexPoolSnapshot) int {
	if snapshot.Config.InitialPerMinute > 0 {
		return snapshot.Config.InitialPerMinute
	}
	return 20
}

func (s *Service) saveAccountsLocked() {
	list := make([]*Account, 0, len(s.accounts))
	for _, account := range s.accounts {
		list = append(list, account)
	}
	data, errMarshal := json.Marshal(list)
	if errMarshal != nil {
		log.WithError(errMarshal).Warn("codex quota: marshal account cache failed")
		return
	}
	if errWrite := writeFileAtomic(filepath.Join(s.dir, accountsFile), data); errWrite != nil {
		log.WithError(errWrite).Warn("codex quota: save account cache failed")
	}
}

// refresh rebuilds the published summary and rows.
func (s *Service) refresh(now time.Time) {
	source := s.source()
	if source == nil || source.HomeEnabled() {
		return
	}
	snapshot := source.CodexPoolSnapshot()
	s.mu.Lock()
	input := capacityInput{Capacity: snapshot.Capacity, Schedulable: snapshot.Schedulable, RPM: snapshot.RPM, FallbackLimit: fallbackLimit(snapshot)}
	summary := buildSummary(s.accounts, s.history, input, s.settings, now)
	summary.Scan = s.scan
	rows := buildRows(s.accounts, now)
	s.mu.Unlock()
	s.summary.Store(&summary)
	s.rows.Store(&rows)
}

// Summary returns the latest report, or nil before the first tick.
func (s *Service) Summary() *Summary { return s.summary.Load() }

// Rows returns the latest per-file quota, keyed by account name.
func (s *Service) Rows() map[string]AccountRow {
	if rows := s.rows.Load(); rows != nil {
		return *rows
	}
	return map[string]AccountRow{}
}

// Settings returns the current options.
func (s *Service) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings
}

// UpdateSettings persists new options and applies them immediately.
func (s *Service) UpdateSettings(next Settings) error {
	if errValidate := next.Validate(); errValidate != nil {
		return errValidate
	}
	data, errMarshal := json.MarshalIndent(next, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("marshal settings: %w", errMarshal)
	}
	if errWrite := writeFileAtomic(filepath.Join(s.dir, settingsFile), data); errWrite != nil {
		return errWrite
	}
	s.mu.Lock()
	s.settings = next
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

// candidatesLocked picks accounts lacking fresh data, oldest first. Accounts
// exhausted until a known reset cannot change, so they are skipped until then.
func (s *Service) candidatesLocked(now time.Time) []string {
	stale := time.Duration(s.settings.StaleMinutes) * time.Minute
	type candidate struct {
		id   string
		last time.Time
	}
	picked := []candidate{}
	seen := map[string]struct{}{}
	ordered := make([]*Account, 0, len(s.accounts))
	for _, account := range s.accounts {
		ordered = append(ordered, account)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	for _, account := range ordered {
		if account.Disabled || account.credentialInvalid || s.isExhaustedUntilReset(account, now) {
			continue
		}
		if _, duplicate := seen[account.key()]; duplicate {
			continue
		}
		seen[account.key()] = struct{}{}
		if account.ScanDenied && now.Sub(account.ScanAt) < invalidRetry {
			continue
		}
		last := account.ObservedAt
		if account.ScanAt.After(last) {
			last = account.ScanAt
		}
		pending := false
		for _, window := range account.Windows {
			pending = pending || window.rolledOver(now)
		}
		if !pending && !last.IsZero() && now.Sub(last) < stale {
			continue
		}
		picked = append(picked, candidate{id: account.ID, last: last})
	}
	sort.SliceStable(picked, func(i, j int) bool { return picked[i].last.Before(picked[j].last) })
	ids := make([]string, len(picked))
	for i, item := range picked {
		ids[i] = item.id
	}
	return ids
}

func (s *Service) isExhaustedUntilReset(account *Account, now time.Time) bool {
	for _, window := range account.Windows {
		if window.UsedPercent >= 100 && window.ResetAt.After(now) {
			return true
		}
	}
	return false
}

func (s *Service) startScan(ctx context.Context) {
	if s.probe == nil || !s.scanBusy.CompareAndSwap(false, true) {
		return
	}
	now := s.now()
	s.mu.Lock()
	enabled, concurrency := s.settings.ScanEnabled, s.settings.ScanConcurrency
	var ids []string
	if enabled {
		ids = s.candidatesLocked(now)
	}
	if len(ids) == 0 {
		s.mu.Unlock()
		s.scanBusy.Store(false)
		return
	}
	s.scan = ScanStatus{Running: true, StartedAt: now, Queued: len(ids)}
	s.mu.Unlock()
	go func() {
		defer s.scanBusy.Store(false)
		s.runScan(ctx, ids, concurrency)
	}()
}

func (s *Service) runScan(ctx context.Context, ids []string, concurrency int) {
	jobs := make(chan string)
	var workers sync.WaitGroup
	for range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for id := range jobs {
				s.scanOne(ctx, id)
			}
		}()
	}
feed:
	for _, id := range ids {
		select {
		case <-ctx.Done():
			break feed
		default:
		}
		// Turning the switch off stops new probes straight away.
		if !s.Settings().ScanEnabled {
			break
		}
		select {
		case jobs <- id:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	workers.Wait()
	s.mu.Lock()
	s.scan.Running = false
	s.scan.Queued = 0
	s.scan.FinishedAt = s.now()
	s.saveAccountsLocked()
	s.mu.Unlock()
}

func (s *Service) scanOne(ctx context.Context, id string) {
	status, body, errProbe := s.probe(ctx, id)
	now := s.now()
	var windows []Window
	plan := ""
	if errProbe == nil && status == http.StatusOK {
		windows, plan, errProbe = ParseUsage(body, now)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scan.Queued > 0 {
		s.scan.Queued--
	}
	account := s.accounts[id]
	if account == nil {
		return
	}
	account.ScanAt = now
	if errProbe == nil && status == http.StatusOK {
		// A response that raced ahead of this probe keeps the newer passive data.
		if now.After(account.ObservedAt) {
			s.applyLocked(account, windows, plan, now, sourceScan)
		}
		s.scan.Succeeded++
		return
	}
	s.scan.Failed++
	if status == http.StatusUnauthorized {
		account.ScanDenied = true
	}
	if errProbe != nil {
		s.scan.LastError = errProbe.Error()
	} else {
		s.scan.LastError = fmt.Sprintf("HTTP %d", status)
	}
}

func writeFileAtomic(path string, data []byte) error {
	temp, errCreate := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if errCreate != nil {
		return fmt.Errorf("create temp file: %w", errCreate)
	}
	name := temp.Name()
	_, errWrite := temp.Write(data)
	errClose := temp.Close()
	if errWrite != nil || errClose != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write %s: %w", filepath.Base(path), errors.Join(errWrite, errClose))
	}
	if errRename := os.Rename(name, path); errRename != nil {
		_ = os.Remove(name)
		return fmt.Errorf("replace %s: %w", filepath.Base(path), errRename)
	}
	return nil
}
