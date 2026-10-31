package auth

import (
	"testing"
	"time"
)

func TestCodexQuotaObservationsCoverOnlyCodexCredentials(t *testing.T) {
	observed := time.Unix(1000, 0)
	m := NewManager(nil, nil, nil)
	m.auths = map[string]*Auth{
		"codex": {ID: "codex", Provider: "codex", FileName: "codex.json", Attributes: map[string]string{"plan_type": "plus"}, Metadata: map[string]any{"account_id": "acct"}, Quota: QuotaState{ObservedAt: observed, Signals: map[string]string{"X": "1"}}},
		"off":   {ID: "off", Provider: "codex", Disabled: true, Metadata: map[string]any{"plan_type": "free"}},
		"dead":  {ID: "dead", Provider: "codex", AuthFailures: codexAuthFailureThreshold},
		"other": {ID: "other", Provider: "gemini"},
	}
	got := map[string]CodexQuotaObservation{}
	for _, item := range m.CodexQuotaObservations() {
		got[item.ID] = item
	}
	if len(got) != 3 {
		t.Fatalf("observations = %v", got)
	}
	if item := got["codex"]; item.Name != "codex.json" || item.Plan != "plus" || item.AccountID != "acct" || !item.ObservedAt.Equal(observed) || item.Signals["X"] != "1" {
		t.Fatalf("codex = %+v", item)
	}
	if item := got["off"]; !item.Disabled || item.Plan != "free" || item.Name != "off" {
		t.Fatalf("off = %+v", item)
	}
	if !got["dead"].Invalid {
		t.Fatalf("dead = %+v", got["dead"])
	}
}
