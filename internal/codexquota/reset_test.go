package codexquota

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func usageBody(used float64, resetAt time.Time, credits int) []byte {
	return []byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":` + strconv.FormatFloat(used, 'f', -1, 64) +
		`,"limit_window_seconds":604800,"reset_at":` + strconv.FormatInt(resetAt.Unix(), 10) + `}},"rate_limit_reset_credits":{"available_count":` + strconv.Itoa(credits) + `}}`)
}

func TestParseResetCountAndExpiry(t *testing.T) {
	if ParseResetCount([]byte(`{}`)) != nil {
		t.Fatal("missing count must stay unknown")
	}
	if got := ParseResetCount([]byte(`{"rate_limit_reset_credits":{"available_count":2}}`)); got == nil || *got != 2 {
		t.Fatalf("count = %v", got)
	}
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	body := []byte(`{"credits":[
		{"status":"redeemed","expires_at":"2026-11-01T00:00:00Z"},
		{"status":"available","is_supported_by_plan":false,"expires_at":"2026-11-02T00:00:00Z"},
		{"status":"available","is_supported_by_plan":true,"expires_at":"2026-10-30T00:00:00Z"},
		{"status":"available","is_supported_by_plan":true,"expires_at":"2026-11-06T21:23:06.797923Z"},
		{"status":"available","is_supported_by_plan":true,"expires_at":"2026-11-05T00:00:00Z"}]}`)
	if got := ParseResetExpiry(body, now); !got.Equal(time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("expiry = %v", got)
	}
}

func TestScanRecordsResetCreditsAndExpiry(t *testing.T) {
	now := time.Now().UTC()
	expires := now.Add(6 * 24 * time.Hour)
	probe := func(context.Context, string) (int, []byte, error) {
		return http.StatusOK, usageBody(100, now.Add(time.Hour), 1), nil
	}
	s := newTestService(t, &fakeSource{}, probe, &now)
	s.SetResetHooks(ResetHooks{Credits: func(context.Context, string) (int, []byte, error) {
		return http.StatusOK, []byte(`{"credits":[{"status":"available","expires_at":"` + expires.Format(time.RFC3339) + `"}]}`), nil
	}})
	s.accounts = map[string]*Account{
		"a": {ID: "a", Name: "a.json", AccountID: "1"},
		"b": {ID: "b", Name: "b.json", AccountID: "1", Disabled: false},
	}
	s.scanOne(context.Background(), "a")

	rows := buildRows(s.accounts, now)
	for _, name := range []string{"a.json", "b.json"} {
		row := rows[name]
		if row.ResetCount == nil || *row.ResetCount != 1 || row.AccountKey != "1" || row.ResetExpiresAt.IsZero() {
			t.Fatalf("%s row = %+v", name, row)
		}
	}
	reset := buildResetSummary(s.accounts, now)
	if reset.Accounts != 1 || reset.Exhausted != 1 || reset.ExpiresAt.IsZero() {
		t.Fatalf("summary = %+v", reset)
	}
	if rows := buildRows(s.accounts, now.Add(resetDataTTL+time.Minute)); rows["a.json"].ResetCount != nil {
		t.Fatal("old reset data must turn unknown")
	}
	if rows := buildRows(s.accounts, expires.Add(time.Minute)); rows["a.json"].ResetCount != nil {
		t.Fatal("expired credit must turn unknown")
	}
}

func resetService(t *testing.T, now *time.Time, credits *atomic.Int32, used *atomic.Int32, consume func() error, cleared *[]string) *Service {
	t.Helper()
	probe := func(context.Context, string) (int, []byte, error) {
		return http.StatusOK, usageBody(float64(used.Load()), now.Add(time.Hour), int(credits.Load())), nil
	}
	s := newTestService(t, &fakeSource{}, probe, now)
	s.SetResetHooks(ResetHooks{
		Consume: func(context.Context, string) error { return consume() },
		ClearCooldown: func(_ context.Context, id string) error {
			*cleared = append(*cleared, id)
			return nil
		},
	})
	s.accounts = map[string]*Account{
		"a": {ID: "a", Name: "a.json", AccountID: "1"},
		"b": {ID: "b", Name: "b.json", AccountID: "1"},
		"c": {ID: "c", Name: "c.json", AccountID: "2"},
	}
	return s
}

func TestResetConsumesOnceAndClearsEveryFileOfTheAccount(t *testing.T) {
	now := time.Now().UTC()
	var credits, used atomic.Int32
	credits.Store(1)
	used.Store(100)
	calls := 0
	cleared := []string{}
	s := resetService(t, &now, &credits, &used, func() error {
		calls++
		credits.Store(0)
		used.Store(0)
		now = now.Add(time.Second)
		return nil
	}, &cleared)

	status, errReset := s.Reset(context.Background(), "a.json")
	if errReset != nil {
		t.Fatalf("Reset() error = %v", errReset)
	}
	if calls != 1 || status.Count != 0 || status.Remaining == nil || *status.Remaining != 100 {
		t.Fatalf("calls = %d status = %+v", calls, status)
	}
	if len(cleared) != 2 {
		t.Fatalf("cleared = %v", cleared)
	}
	if _, errAgain := s.Reset(context.Background(), "b.json"); !errors.Is(errAgain, ErrNoResetCredit) {
		t.Fatalf("second reset error = %v", errAgain)
	}
	if calls != 1 {
		t.Fatalf("a spent credit was consumed again, calls = %d", calls)
	}
}

func TestResetRejectsUnknownBusyAndFailedConsume(t *testing.T) {
	now := time.Now().UTC()
	var credits, used atomic.Int32
	credits.Store(1)
	used.Store(100)
	cleared := []string{}
	failure := &UpstreamError{Status: http.StatusForbidden}
	s := resetService(t, &now, &credits, &used, func() error { return failure }, &cleared)

	if _, errReset := s.Reset(context.Background(), "missing.json"); !errors.Is(errReset, ErrAccountNotFound) {
		t.Fatalf("unknown error = %v", errReset)
	}
	s.resetting["1"] = struct{}{}
	if _, errReset := s.Reset(context.Background(), "b.json"); !errors.Is(errReset, ErrResetBusy) {
		t.Fatalf("busy error = %v", errReset)
	}
	delete(s.resetting, "1")
	if _, errReset := s.Reset(context.Background(), "a.json"); !errors.Is(errReset, failure) {
		t.Fatalf("consume error = %v", errReset)
	}
	if len(cleared) != 0 || len(s.resetting) != 0 {
		t.Fatalf("cleared = %v resetting = %v", cleared, s.resetting)
	}
}

func TestCheckResetMarksDeniedAccount(t *testing.T) {
	now := time.Now().UTC()
	s := newTestService(t, &fakeSource{}, func(context.Context, string) (int, []byte, error) {
		return http.StatusUnauthorized, nil, nil
	}, &now)
	s.accounts = map[string]*Account{"a": {ID: "a", Name: "a.json"}}
	_, errCheck := s.CheckReset(context.Background(), "a.json")
	var upstream *UpstreamError
	if !errors.As(errCheck, &upstream) || upstream.Status != http.StatusUnauthorized || !s.accounts["a"].ScanDenied {
		t.Fatalf("error = %v account = %+v", errCheck, s.accounts["a"])
	}
}
