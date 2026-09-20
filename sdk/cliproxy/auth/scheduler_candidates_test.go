package auth

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestSchedulerAttributeSensitiveWarmLookup(t *testing.T) {
	for key, want := range map[string]bool{
		"project_id": false, "priority": false, " API.Key ": true,
		"x-access-token": true, "Proxy URL": true, "Auth.Header": true,
	} {
		if got := schedulerAttributeSensitive(key); got != want {
			t.Fatalf("sensitive(%q) = %v, want %v", key, got, want)
		}
	}
	schedulerAttributeSensitive("project_id")
	if got := testing.AllocsPerRun(100, func() { schedulerAttributeSensitive("project_id") }); got != 0 {
		t.Fatalf("warm key check allocates %g objects", got)
	}
}

func TestSchedulerAttributeCacheDoesNotRetainOversizedKeys(t *testing.T) {
	key := strings.Repeat("long_attribute_", 100)
	schedulerAttributeSensitive(key)
	if _, ok := schedulerAttributeSensitivity.Load(key); ok {
		t.Fatal("oversized attribute key was retained")
	}
}

type inactiveReviewScheduler struct{ calls int }

func (*inactiveReviewScheduler) HasScheduler() bool { return false }
func (s *inactiveReviewScheduler) PickAuth(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	s.calls++
	return pluginapi.SchedulerPickResponse{}, false, nil
}

func TestPluginSchedulerInactiveHostSkipsCandidates(t *testing.T) {
	m := &Manager{}
	scheduler := &inactiveReviewScheduler{}
	_, handled, err := m.pickViaPluginScheduler(context.Background(), scheduler, "antigravity", nil, "model", cliproxyexecutor.Options{}, nil, []*Auth{{ID: "fixture"}})
	if err != nil || handled || scheduler.calls != 0 {
		t.Fatalf("inactive scheduler called: calls=%d, handled=%v, err=%v", scheduler.calls, handled, err)
	}
}

func BenchmarkSchedulerAttributeSensitive(b *testing.B) {
	schedulerAttributeSensitive("project_id")
	b.ReportAllocs()
	for b.Loop() {
		schedulerAttributeSensitive("project_id")
	}
}

func benchmarkSchedulerAuths() []*Auth {
	auths := make([]*Auth, 1000)
	keys := []string{"project_id", "priority", "region", "email", "label", "type", "source", "websocket", "account_id", "model", "api_key", "access_token"}
	for i := range auths {
		attrs := make(map[string]string, len(keys))
		for _, key := range keys {
			attrs[key] = "fixture"
		}
		auths[i] = &Auth{ID: fmt.Sprint(i), Provider: "antigravity", Status: StatusActive, Attributes: attrs, RegistrationEpoch: 1, Generation: 1}
	}
	return auths
}

func BenchmarkSchedulerCandidates1000(b *testing.B) {
	auths := benchmarkSchedulerAuths()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		schedulerAuthCandidates(auths)
	}
}

func TestSchedulerCandidatesCacheVersions(t *testing.T) {
	m := &Manager{}
	auth := &Auth{ID: "fixture", Provider: "antigravity", Status: StatusActive, RegistrationEpoch: 1, Generation: 1, Attributes: map[string]string{"project_id": "first", "access_token": "secret"}}
	first := m.cachedSchedulerCandidates([]*Auth{auth})[0]
	second := m.cachedSchedulerCandidates([]*Auth{auth})[0]
	if reflect.ValueOf(first.Attributes).Pointer() != reflect.ValueOf(second.Attributes).Pointer() {
		t.Fatal("unchanged credential rebuilt attributes")
	}
	if _, ok := first.Attributes["access_token"]; ok {
		t.Fatal("sensitive attribute leaked")
	}
	auth.Generation++
	auth.Attributes["project_id"] = "updated"
	auth.Status = StatusError
	updated := m.cachedSchedulerCandidates([]*Auth{auth})[0]
	if updated.Attributes["project_id"] != "updated" || updated.Status != string(StatusError) || first.Attributes["project_id"] != "first" {
		t.Fatalf("bad generation update: first=%+v, updated=%+v", first, updated)
	}
	auth.RegistrationEpoch++
	auth.Generation = 1
	auth.Attributes["project_id"] = "registered-again"
	if got := m.cachedSchedulerCandidates([]*Auth{auth})[0].Attributes["project_id"]; got != "registered-again" {
		t.Fatalf("re-registration reused stale attributes: %q", got)
	}
	stale := auth.Clone()
	stale.RegistrationEpoch--
	stale.Generation = 999
	stale.Attributes["project_id"] = "stale"
	m.cachedSchedulerCandidates([]*Auth{stale})
	if got := m.cachedSchedulerCandidates([]*Auth{auth})[0].Attributes["project_id"]; got != "registered-again" {
		t.Fatalf("stale snapshot displaced fresh registration: %q", got)
	}
}

func TestSchedulerCandidatesCacheCapacityPreservesReuse(t *testing.T) {
	m := &Manager{}
	auths := make([]*Auth, maxSchedulerCandidateCacheEntries+1)
	for i := range auths {
		auths[i] = &Auth{ID: fmt.Sprint(i), RegistrationEpoch: 1, Generation: 1, Attributes: map[string]string{"project_id": "fixture"}}
	}
	first := m.cachedSchedulerCandidates(auths)
	second := m.cachedSchedulerCandidates(auths)
	for i := range maxSchedulerCandidateCacheEntries {
		if reflect.ValueOf(first[i].Attributes).Pointer() != reflect.ValueOf(second[i].Attributes).Pointer() {
			t.Fatalf("cache capacity evicted reusable entry %d", i)
		}
	}
	if len(m.schedulerCandidates) > maxSchedulerCandidateCacheEntries {
		t.Fatal("candidate cache exceeded capacity")
	}
}

func TestSchedulerCandidatesWithoutVersionsAreNotCached(t *testing.T) {
	m := &Manager{}
	auth := &Auth{ID: "fixture", Attributes: map[string]string{"project_id": "first"}}
	m.cachedSchedulerCandidates([]*Auth{auth})
	auth.Attributes["project_id"] = "second"
	if got := m.cachedSchedulerCandidates([]*Auth{auth})[0].Attributes["project_id"]; got != "second" {
		t.Fatalf("unversioned credential reused stale attributes: %q", got)
	}
}

func TestSchedulerMetadataExcludesSessionCache(t *testing.T) {
	metadata := map[string]any{cliproxyexecutor.SessionInfoCacheMetadataKey: func() {}, "public": "value"}
	got := cloneSchedulerAnyMap(metadata)
	if len(got) != 1 || got["public"] != "value" {
		t.Fatalf("unexpected plugin metadata: %v", got)
	}
}

func BenchmarkSchedulerCandidates1000Cached(b *testing.B) {
	m := &Manager{}
	auths := benchmarkSchedulerAuths()
	m.cachedSchedulerCandidates(auths)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		m.cachedSchedulerCandidates(auths)
	}
}
