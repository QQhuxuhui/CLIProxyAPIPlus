package codexquota

import (
	"math"
	"sort"
	"time"
)

const (
	// usableFloorPercent is the remaining quota below which an account adds no capacity.
	usableFloorPercent = 5
	recoverWindow      = 24 * time.Hour
	maxForecastHours   = 720
)

// WindowStat summarizes one window length within a plan.
type WindowStat struct {
	Minutes           int      `json:"minutes"`
	Accounts          int      `json:"accounts"`
	AvgRemaining      float64  `json:"avg_remaining"`
	BurnPerHour1h     *float64 `json:"burn_per_hour_1h"`
	BurnPerHour24h    *float64 `json:"burn_per_hour_24h"`
	RunwayHours       *float64 `json:"runway_hours"`
	ShortfallAccounts int      `json:"shortfall_accounts"`
}

// PlanStat summarizes one plan. Distribution buckets remaining quota into
// 0-10, 10-30, 30-60 and 60-100 percent.
type PlanStat struct {
	Plan              string       `json:"plan"`
	Accounts          int          `json:"accounts"`
	Known             int          `json:"known"`
	Unknown           int          `json:"unknown"`
	Usable            int          `json:"usable"`
	Pending           int          `json:"pending"`
	AvgRemaining      float64      `json:"avg_remaining"`
	Distribution      [4]int       `json:"distribution"`
	Recover24h        int          `json:"recover_24h"`
	Windows           []WindowStat `json:"windows"`
	ShortfallAccounts int          `json:"shortfall_accounts"`
}

// Advice is the add-accounts recommendation.
type Advice struct {
	RecommendedAccounts int      `json:"recommended_accounts"`
	RPMShortfall        int      `json:"rpm_shortfall"`
	QuotaShortfall      int      `json:"quota_shortfall"`
	PeakRPM             int      `json:"peak_rpm"`
	TargetRPM           int      `json:"target_rpm"`
	UsableCapacity      int      `json:"usable_capacity"`
	RPMRemaining        int      `json:"rpm_remaining"`
	AvgAccountLimit     float64  `json:"avg_account_limit"`
	RunwayHours         *float64 `json:"runway_hours"`
	WarmingUp           bool     `json:"warming_up"`
	LowConfidence       bool     `json:"low_confidence"`
}

// TrendPoint is one downsampled pool history entry.
type TrendPoint struct {
	Time         time.Time `json:"time"`
	Usable       int       `json:"usable"`
	Capacity     int       `json:"capacity"`
	PeakRPM      int       `json:"peak_rpm"`
	AvgRemaining float64   `json:"avg_remaining"`
}

// Summary is the pool quota report.
type Summary struct {
	Available      bool         `json:"available"`
	HomeMode       bool         `json:"home_mode"`
	GeneratedAt    time.Time    `json:"generated_at"`
	Files          int          `json:"files"`
	UniqueAccounts int          `json:"unique_accounts"`
	DuplicateFiles int          `json:"duplicate_files"`
	Disabled       int          `json:"disabled"`
	Invalid        int          `json:"invalid"`
	Usable         int          `json:"usable"`
	Unknown        int          `json:"unknown"`
	Plans          []PlanStat   `json:"plans"`
	Advice         Advice       `json:"advice"`
	Reset          ResetSummary `json:"reset"`
	Trend          []TrendPoint `json:"trend"`
	Settings       Settings     `json:"settings"`
	Scan           ScanStatus   `json:"scan"`
}

// AccountRow is one credential file in the account list.
type AccountRow struct {
	Plan       string      `json:"plan"`
	Remaining  *float64    `json:"remaining"`
	Windows    []RowWindow `json:"windows"`
	ObservedAt time.Time   `json:"observed_at,omitempty"`
	Source     string      `json:"source,omitempty"`
	Duplicate  bool        `json:"duplicate"`
	Invalid    bool        `json:"invalid"`
	// AccountKey groups files sharing one upstream account and its reset credits.
	AccountKey     string    `json:"account_key"`
	ResetCount     *int      `json:"reset_count"`
	ResetExpiresAt time.Time `json:"reset_expires_at,omitempty"`
}

// ResetSummary counts redeemable resets across unique accounts.
type ResetSummary struct {
	Accounts  int       `json:"accounts"`
	Exhausted int       `json:"exhausted"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// RowWindow is a window with its reset rolled forward to now.
type RowWindow struct {
	Minutes     int       `json:"minutes"`
	UsedPercent float64   `json:"used_percent"`
	ResetAt     time.Time `json:"reset_at,omitempty"`
	Pending     bool      `json:"pending"`
}

type capacityInput struct {
	Capacity      int
	Schedulable   int
	RPM           int
	FallbackLimit int
}

type forecastInput struct {
	used    float64
	resetAt time.Time
}

// remainingOf returns the tightest window and whether any window awaits confirmation.
func remainingOf(windows []Window, now time.Time) (float64, bool) {
	remaining, pending := 100.0, false
	for _, window := range windows {
		remaining = math.Min(remaining, window.remaining(now))
		pending = pending || window.rolledOver(now)
	}
	return remaining, pending
}

func uniqueAccounts(accounts map[string]*Account) map[string]*Account {
	unique := make(map[string]*Account, len(accounts))
	for _, account := range accounts {
		if account.Disabled || account.isInvalid() {
			continue
		}
		current, ok := unique[account.key()]
		if !ok || account.ObservedAt.After(current.ObservedAt) {
			unique[account.key()] = account
		}
	}
	return unique
}

// forecast walks the budget forward with resets refilling it. Usage after now is
// never refilled inside the window, so long horizons err on the pessimistic side.
func forecast(items []forecastInput, burnPerHour float64, now time.Time, windowMinutes, horizonHours int) (*float64, int) {
	if burnPerHour <= 0 {
		return nil, 0
	}
	budget := 0.0
	sorted := make([]forecastInput, 0, len(items))
	for _, item := range items {
		budget += 100 - item.used
		if !item.resetAt.IsZero() && item.used > 0 {
			sorted = append(sorted, item)
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].resetAt.Before(sorted[j].resetAt) })
	endHours := math.Min(float64(windowMinutes)/60, maxForecastHours)
	shortfallHours := math.Min(endHours, float64(horizonHours))
	step := math.Min(1, endHours/24)
	var runway *float64
	lowest := budget
	next := 0
	for hours := step; hours <= endHours+1e-9; hours += step {
		budget -= burnPerHour * step
		at := now.Add(time.Duration(hours * float64(time.Hour)))
		for next < len(sorted) && !sorted[next].resetAt.After(at) {
			budget += sorted[next].used
			next++
		}
		if budget <= 0 && runway == nil {
			value := hours
			runway = &value
		}
		if hours <= shortfallHours+1e-9 {
			lowest = math.Min(lowest, budget)
		}
	}
	if lowest >= 0 {
		return runway, 0
	}
	return runway, int(math.Ceil(-lowest / 100))
}

func buildSummary(accounts map[string]*Account, history *history, capacity capacityInput, settings Settings, now time.Time) Summary {
	out := Summary{Available: true, GeneratedAt: now, Files: len(accounts), Plans: []PlanStat{}, Settings: settings}
	seen := map[string]int{}
	for _, account := range accounts {
		seen[account.key()]++
		switch {
		case account.Disabled:
			out.Disabled++
		case account.isInvalid():
			out.Invalid++
		}
	}
	for _, count := range seen {
		if count > 1 {
			out.DuplicateFiles += count - 1
		}
	}
	unique := uniqueAccounts(accounts)
	out.UniqueAccounts = len(unique)

	plans := map[string]*PlanStat{}
	totals := map[string]float64{}
	windowTotals := map[string]map[int]float64{}
	windowItems := map[string]map[int][]forecastInput{}
	for _, account := range unique {
		name := account.Plan
		if name == "" {
			name = "unknown"
		}
		plan := plans[name]
		if plan == nil {
			plan = &PlanStat{Plan: name}
			plans[name] = plan
			windowTotals[name] = map[int]float64{}
			windowItems[name] = map[int][]forecastInput{}
		}
		plan.Accounts++
		if len(account.Windows) == 0 {
			plan.Unknown++
			out.Unknown++
			continue
		}
		plan.Known++
		remaining, pending := remainingOf(account.Windows, now)
		totals[name] += remaining
		if pending {
			plan.Pending++
		}
		if remaining >= usableFloorPercent {
			plan.Usable++
			out.Usable++
		} else if recovers(account.Windows, now) {
			plan.Recover24h++
		}
		plan.Distribution[distributionBucket(remaining)]++
		for _, window := range account.Windows {
			used := 100 - window.remaining(now)
			resetAt := window.ResetAt
			if window.rolledOver(now) {
				resetAt = time.Time{}
			}
			windowTotals[name][window.Minutes] += window.remaining(now)
			windowItems[name][window.Minutes] = append(windowItems[name][window.Minutes], forecastInput{used: used, resetAt: resetAt})
		}
	}

	out.Reset = buildResetSummary(accounts, now)

	covered := history.covered(now)
	warming := covered < 24*time.Hour
	var runway *float64
	for name, plan := range plans {
		if plan.Known > 0 {
			plan.AvgRemaining = round1(totals[name] / float64(plan.Known))
		}
		plan.Windows = []WindowStat{}
		for minutes, items := range windowItems[name] {
			stat := WindowStat{Minutes: minutes, Accounts: len(items), AvgRemaining: round1(windowTotals[name][minutes] / float64(len(items)))}
			stat.BurnPerHour1h = history.burnRate(name, minutes, time.Hour, now)
			stat.BurnPerHour24h = history.burnRate(name, minutes, 24*time.Hour, now)
			burn := 0.0
			if stat.BurnPerHour1h != nil {
				burn = *stat.BurnPerHour1h
			}
			if stat.BurnPerHour24h != nil {
				burn = math.Max(burn, *stat.BurnPerHour24h)
			}
			stat.RunwayHours, stat.ShortfallAccounts = forecast(items, burn, now, minutes, settings.HorizonHours)
			plan.ShortfallAccounts = max(plan.ShortfallAccounts, stat.ShortfallAccounts)
			if stat.RunwayHours != nil && (runway == nil || *stat.RunwayHours < *runway) {
				runway = stat.RunwayHours
			}
			plan.Windows = append(plan.Windows, stat)
		}
		sort.Slice(plan.Windows, func(i, j int) bool { return plan.Windows[i].Minutes < plan.Windows[j].Minutes })
		out.Plans = append(out.Plans, *plan)
		out.Advice.QuotaShortfall += plan.ShortfallAccounts
	}
	sort.Slice(out.Plans, func(i, j int) bool { return out.Plans[i].Accounts > out.Plans[j].Accounts })

	limit := float64(capacity.FallbackLimit)
	if capacity.Schedulable > 0 && capacity.Capacity > 0 {
		limit = float64(capacity.Capacity) / float64(capacity.Schedulable)
	}
	peak := max(history.peakRPM(24*time.Hour, now), capacity.RPM)
	usableCapacity := int(float64(out.Usable+out.Unknown) * limit)
	target := int(math.Ceil(float64(peak) * (1 + settings.HeadroomPercent/100)))
	out.Advice.AvgAccountLimit = round1(limit)
	out.Advice.PeakRPM = peak
	out.Advice.TargetRPM = target
	out.Advice.UsableCapacity = usableCapacity
	out.Advice.RPMRemaining = usableCapacity - target
	if target > usableCapacity && limit > 0 {
		out.Advice.RPMShortfall = int(math.Ceil(float64(target-usableCapacity) / limit))
	}
	out.Advice.RecommendedAccounts = max(out.Advice.RPMShortfall, out.Advice.QuotaShortfall)
	out.Advice.RunwayHours = runway
	out.Advice.WarmingUp = warming
	out.Advice.LowConfidence = out.UniqueAccounts > 0 && float64(out.Unknown)/float64(out.UniqueAccounts) > 0.2
	out.Trend = history.trend(24*time.Hour, now)
	return out
}

func recovers(windows []Window, now time.Time) bool {
	limit := now.Add(recoverWindow)
	for _, window := range windows {
		if window.remaining(now) >= usableFloorPercent {
			continue
		}
		if window.ResetAt.IsZero() || window.ResetAt.After(limit) {
			return false
		}
	}
	return true
}

func distributionBucket(remaining float64) int {
	switch {
	case remaining < 10:
		return 0
	case remaining < 30:
		return 1
	case remaining < 60:
		return 2
	default:
		return 3
	}
}

func round1(value float64) float64 { return math.Round(value*10) / 10 }

func buildResetSummary(accounts map[string]*Account, now time.Time) ResetSummary {
	out := ResetSummary{}
	for _, account := range resetSources(accounts, now) {
		if *account.ResetCount <= 0 {
			continue
		}
		out.Accounts++
		if remaining, _ := remainingOf(account.Windows, now); remaining < usableFloorPercent {
			out.Exhausted++
		}
		if expires := account.ResetExpiresAt; !expires.IsZero() && (out.ExpiresAt.IsZero() || expires.Before(out.ExpiresAt)) {
			out.ExpiresAt = expires
		}
	}
	return out
}

func buildRows(accounts map[string]*Account, now time.Time) map[string]AccountRow {
	resets := resetSources(accounts, now)
	counts := map[string]int{}
	newest := map[string]*Account{}
	for _, account := range accounts {
		key := account.key()
		counts[key]++
		if current, ok := newest[key]; !ok || account.ObservedAt.After(current.ObservedAt) {
			newest[key] = account
		}
	}
	rows := make(map[string]AccountRow, len(accounts))
	for _, account := range accounts {
		// Files of one account share a quota, so each shows the newest reading.
		source := newest[account.key()]
		row := AccountRow{Plan: source.Plan, ObservedAt: source.ObservedAt, Source: source.Source, Duplicate: counts[account.key()] > 1, Invalid: account.isInvalid(), Windows: []RowWindow{}, AccountKey: account.key()}
		if reset, ok := resets[account.key()]; ok {
			count := *reset.ResetCount
			row.ResetCount = &count
			row.ResetExpiresAt = reset.ResetExpiresAt
		}
		if len(source.Windows) > 0 {
			remaining, _ := remainingOf(source.Windows, now)
			row.Remaining = &remaining
		}
		for _, window := range source.Windows {
			item := RowWindow{Minutes: window.Minutes, UsedPercent: 100 - window.remaining(now), ResetAt: window.ResetAt}
			if window.rolledOver(now) {
				item.ResetAt = time.Time{}
				item.Pending = true
			}
			row.Windows = append(row.Windows, item)
		}
		rows[account.Name] = row
	}
	return rows
}
