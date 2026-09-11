package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

type antigravityStreamCallResult struct {
	result *cliproxyexecutor.StreamResult
	err    error
}

type antigravityCallResult struct {
	response cliproxyexecutor.Response
	err      error
}

func TestAntigravityExecutorSameSessionNonStreamTurnsAreSerialized(t *testing.T) {
	entered := make(chan string, 2)
	releases := map[string]chan struct{}{"first": make(chan struct{}), "second": make(chan struct{})}
	var releaseFirstOnce sync.Once
	var releaseSecondOnce sync.Once
	defer releaseFirstOnce.Do(func() { close(releases["first"]) })
	defer releaseSecondOnce.Do(func() { close(releases["second"]) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		text := gjson.GetBytes(body, "request.contents.0.parts.0.text").String()
		entered <- text
		<-releases[text]
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, "{\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":%q}]},\"finishReason\":\"STOP\"}]}}", text)
	}))
	defer server.Close()

	exec, auth := antigravityConcurrencyFixture(server.URL)
	firstReq, firstOpts := antigravityConcurrencyRequest("stable-session", "first")
	firstOpts.Stream = false
	firstCall := startAntigravityCall(exec, auth, firstReq, firstOpts)
	if got := receiveString(t, entered); got != "first" {
		t.Fatalf("first upstream body = %q", got)
	}

	secondReq, secondOpts := antigravityConcurrencyRequest("stable-session", "second")
	secondOpts.Stream = false
	secondCall := startAntigravityCall(exec, auth, secondReq, secondOpts)
	_, upstreamID, _ := antigravitySessionForRequest(context.Background(), secondReq, secondOpts)
	waitForAntigravityLockWaiterOrUpstreamEntry(t, exec.sessionLocks(), upstreamID, entered)

	releaseFirstOnce.Do(func() { close(releases["first"]) })
	receiveAntigravityCall(t, firstCall)
	if got := receiveString(t, entered); got != "second" {
		t.Fatalf("second upstream body = %q", got)
	}
	releaseSecondOnce.Do(func() { close(releases["second"]) })
	receiveAntigravityCall(t, secondCall)
}

func TestAntigravityExecutorSameSessionTurnsAreSerialized(t *testing.T) {
	entered := make(chan string, 2)
	releases := map[string]chan struct{}{"first": make(chan struct{}), "second": make(chan struct{})}
	var releaseFirstOnce sync.Once
	var releaseSecondOnce sync.Once
	defer releaseFirstOnce.Do(func() { close(releases["first"]) })
	defer releaseSecondOnce.Do(func() { close(releases["second"]) })
	var bodiesMu sync.Mutex
	bodies := make(map[string]string)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			return
		}
		text := gjson.GetBytes(body, "request.contents.0.parts.0.text").String()
		bodiesMu.Lock()
		bodies[text] = string(body)
		bodiesMu.Unlock()
		entered <- text
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-releases[text]
		_, _ = fmt.Fprintf(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":%q}]},\"finishReason\":\"STOP\"}]}}\n\n", text)
	}))
	defer server.Close()

	exec, auth := antigravityConcurrencyFixture(server.URL)
	firstReq, firstOpts := antigravityConcurrencyRequest("stable-session", "first")
	firstCall := startAntigravityStreamCall(exec, auth, firstReq, firstOpts)
	if got := receiveString(t, entered); got != "first" {
		t.Fatalf("first upstream body = %q", got)
	}
	first := receiveAntigravityStreamCall(t, firstCall)

	secondReq, secondOpts := antigravityConcurrencyRequest("stable-session", "second")
	secondCall := startAntigravityStreamCall(exec, auth, secondReq, secondOpts)
	_, upstreamID, _ := antigravitySessionForRequest(context.Background(), secondReq, secondOpts)
	waitForAntigravityLockWaiterOrUpstreamEntry(t, exec.sessionLocks(), upstreamID, entered)

	releaseFirstOnce.Do(func() { close(releases["first"]) })
	drainAntigravityStream(t, first)
	if got := receiveString(t, entered); got != "second" {
		t.Fatalf("second upstream body = %q", got)
	}
	second := receiveAntigravityStreamCall(t, secondCall)
	releaseSecondOnce.Do(func() { close(releases["second"]) })
	drainAntigravityStream(t, second)

	bodiesMu.Lock()
	defer bodiesMu.Unlock()
	if !gjson.Valid(bodies["first"]) || !gjson.Valid(bodies["second"]) || bodies["first"] == bodies["second"] {
		t.Fatalf("upstream request bodies were not kept distinct: first=%s second=%s", bodies["first"], bodies["second"])
	}
}

func TestAntigravityExecutorDifferentSessionsRemainConcurrent(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		entered <- gjson.GetBytes(body, "request.contents.0.parts.0.text").String()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-release
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}}\n\n")
	}))
	defer server.Close()

	exec, auth := antigravityConcurrencyFixture(server.URL)
	firstReq, firstOpts := antigravityConcurrencyRequest("session-a", "first")
	firstCall := startAntigravityStreamCall(exec, auth, firstReq, firstOpts)
	_ = receiveString(t, entered)
	first := receiveAntigravityStreamCall(t, firstCall)

	secondReq, secondOpts := antigravityConcurrencyRequest("session-b", "second")
	secondCall := startAntigravityStreamCall(exec, auth, secondReq, secondOpts)
	if got := receiveString(t, entered); got != "second" {
		t.Fatalf("second independent upstream body = %q", got)
	}
	second := receiveAntigravityStreamCall(t, secondCall)
	releaseOnce.Do(func() { close(release) })
	drainAntigravityStream(t, first)
	drainAntigravityStream(t, second)
}

func antigravityConcurrencyFixture(baseURL string) (*AntigravityExecutor, *cliproxyauth.Auth) {
	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	auth := &cliproxyauth.Auth{
		ID:       "antigravity-concurrency-auth",
		Provider: "antigravity",
		Attributes: map[string]string{
			"base_url": baseURL,
		},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	return exec, auth
}

func antigravityConcurrencyRequest(sessionID, text string) (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	payload := []byte(fmt.Sprintf(`{"session_id":%q,"request":{"contents":[{"role":"user","parts":[{"text":%q}]}]}}`, sessionID, text))
	req := cliproxyexecutor.Request{Model: "gemini-3-flash-agent", Payload: payload}
	opts := cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FormatAntigravity,
		ResponseFormat:  sdktranslator.FormatAntigravity,
		OriginalRequest: payload,
	}
	return req, opts
}

func startAntigravityStreamCall(exec *AntigravityExecutor, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) <-chan antigravityStreamCallResult {
	out := make(chan antigravityStreamCallResult, 1)
	go func() {
		result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
		out <- antigravityStreamCallResult{result: result, err: err}
	}()
	return out
}

func startAntigravityCall(exec *AntigravityExecutor, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) <-chan antigravityCallResult {
	out := make(chan antigravityCallResult, 1)
	go func() {
		response, err := exec.Execute(context.Background(), auth, req, opts)
		out <- antigravityCallResult{response: response, err: err}
	}()
	return out
}

func receiveAntigravityCall(t *testing.T, call <-chan antigravityCallResult) cliproxyexecutor.Response {
	t.Helper()
	select {
	case got := <-call:
		if got.err != nil {
			t.Fatal(got.err)
		}
		return got.response
	case <-time.After(2 * time.Second):
		t.Fatal("non-stream call did not return")
		return cliproxyexecutor.Response{}
	}
}

func receiveAntigravityStreamCall(t *testing.T, call <-chan antigravityStreamCallResult) *cliproxyexecutor.StreamResult {
	t.Helper()
	select {
	case got := <-call:
		if got.err != nil {
			t.Fatal(got.err)
		}
		return got.result
	case <-time.After(2 * time.Second):
		t.Fatal("stream call did not return")
		return nil
	}
}

func receiveString(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream entry")
		return ""
	}
}

func waitForAntigravityLockWaiterOrUpstreamEntry(t *testing.T, manager *antigravitySessionLockManager, key string, entered <-chan string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case got := <-entered:
			t.Fatalf("same-session request entered upstream concurrently: %q", got)
		case <-deadline:
			t.Fatal("second request neither waited on the session lock nor entered upstream")
		default:
			if manager.refCount(key) == 2 {
				return
			}
			runtime.Gosched()
		}
	}
}

func drainAntigravityStream(t *testing.T, result *cliproxyexecutor.StreamResult) {
	t.Helper()
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}
}
