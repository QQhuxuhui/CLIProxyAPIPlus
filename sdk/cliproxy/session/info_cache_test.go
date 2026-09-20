package session

import (
	"bytes"
	"fmt"
	"net/http"
	"sync"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestSessionInfoCacheTracksInputs(t *testing.T) {
	metadata := WithInfoCache(nil)
	headers := http.Header{"X-Session-Id": {"first"}}
	payload := []byte(`{"parent_session_id":"parent"}`)
	check := func(wantID, wantParent string) {
		t.Helper()
		info, ok := ExtractSessionInfo(headers, payload, metadata)
		if !ok || info.SessionID != wantID || info.ParentSessionID != wantParent {
			t.Fatalf("session = %+v, %v; want %q, %q", info, ok, wantID, wantParent)
		}
	}
	check("header:first", "header:parent")
	check("header:first", "header:parent")
	headers.Set("X-Session-Id", "second")
	check("header:second", "header:parent")
	payload = []byte(`{"parent_session_id":"other"}`)
	check("header:second", "header:other")
	headers.Del("X-Session-Id")
	payload = []byte(`{}`)
	for range 2 {
		if info, ok := ExtractSessionInfo(headers, payload, metadata); ok {
			t.Fatalf("expected cached missing session, got %+v", info)
		}
	}
	metadata[cliproxyexecutor.ExecutionSessionMetadataKey] = "execution"
	check("execution:execution", "")
	metadata[cliproxyexecutor.CallerScopeMetadataKey] = "caller"
	info, _ := ExtractSessionInfo(headers, payload, metadata)
	if info.CallerScope != "caller" {
		t.Fatalf("stale caller scope: %+v", info)
	}
}

func TestSessionInfoCacheConcurrentReads(t *testing.T) {
	metadata := WithInfoCache(nil)
	payload := []byte(`{"session_id":"fixture"}`)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 20 {
				info, ok := ExtractSessionInfo(nil, payload, metadata)
				if !ok || info.SessionID != "session:fixture" {
					t.Errorf("unexpected session: %+v", info)
				}
			}
		})
	}
	wg.Wait()
}

func TestSessionInfoCacheWarmExtractionDoesNotAllocate(t *testing.T) {
	metadata := WithInfoCache(nil)
	headers := http.Header{"X-Session-Id": {"fixture"}}
	payload := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	ExtractSessionInfo(headers, payload, metadata)
	allocations := testing.AllocsPerRun(100, func() {
		ExtractSessionInfo(headers, payload, metadata)
	})
	if allocations != 0 {
		t.Fatalf("warm extraction allocated %g objects; want 0", allocations)
	}
}

func TestSessionInfoDuplicateKeysKeepFirst(t *testing.T) {
	payload := []byte(`{"session_id":"first","session_id":"second","metadata":{"parent_id":"parent"},"metadata":{"parent_id":"other"}}`)
	info, ok := ExtractSessionInfo(nil, payload, nil)
	if !ok || info.SessionID != "session:first" || info.ParentSessionID != "session:parent" {
		t.Fatalf("duplicate key precedence changed: %+v", info)
	}
}

func TestSessionInfoDuplicateContainersContinueNestedSearch(t *testing.T) {
	for _, tc := range []struct{ payload, session, parent string }{
		{`{"metadata":{},"metadata":{"session_id":"second"}}`, "session:second", ""},
		{`{"session_id":"child","metadata":{},"metadata":{"parent_session_id":"parent"}}`, "session:child", "session:parent"},
	} {
		info, ok := ExtractSessionInfo(nil, []byte(tc.payload), nil)
		if !ok || info.SessionID != tc.session || info.ParentSessionID != tc.parent {
			t.Fatalf("session for %s = %+v, %v", tc.payload, info, ok)
		}
	}
}

func BenchmarkSessionInfoLargePayload(b *testing.B) {
	for _, size := range []int{1024, 2 << 20} {
		payload := append([]byte(`{"contents":[{"parts":[{"inlineData":{"data":"`), bytes.Repeat([]byte("A"), size)...)
		payload = append(payload, []byte(`"}}]}]}`)...)
		for _, cached := range []bool{false, true} {
			for _, header := range []bool{false, true} {
				b.Run(fmt.Sprintf("bytes_%d/cached_%t/header_%t", size, cached, header), func(b *testing.B) {
					var metadata map[string]any
					if cached {
						metadata = WithInfoCache(nil)
					}
					var headers http.Header
					if header {
						headers = http.Header{"X-Session-Id": {"fixture"}}
					}
					ExtractSessionInfo(headers, payload, metadata)
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						ExtractSessionInfo(headers, payload, metadata)
					}
				})
			}
		}
	}
}
