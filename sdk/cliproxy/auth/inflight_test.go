package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestBeginInFlightCountsAndReleasesOnce(t *testing.T) {
	m := &Manager{}
	release := m.beginInFlight("a")
	m.beginInFlight("a")
	if got := m.InFlight("a"); got != 2 {
		t.Fatalf("in-flight = %d, want 2", got)
	}
	release()
	release()
	if got := m.InFlight("a"); got != 1 {
		t.Fatalf("double release must not double-decrement, got %d", got)
	}
	if snap := m.InFlightSnapshot(); snap["a"] != 1 || len(snap) != 1 {
		t.Fatalf("snapshot = %v", snap)
	}
}

func TestWrapInFlightStreamReleasesWhenUpstreamCloses(t *testing.T) {
	m := &Manager{}
	release := m.beginInFlight("a")
	src := make(chan cliproxyexecutor.StreamChunk, 2)
	wrapped := wrapInFlightStream(context.Background(), &cliproxyexecutor.StreamResult{Chunks: src}, release)
	src <- cliproxyexecutor.StreamChunk{Payload: []byte("x")}
	close(src)
	if chunk, ok := <-wrapped.Chunks; !ok || string(chunk.Payload) != "x" {
		t.Fatalf("chunk not forwarded: ok=%v payload=%q", ok, chunk.Payload)
	}
	if _, ok := <-wrapped.Chunks; ok {
		t.Fatalf("wrapped channel should close after upstream closes")
	}
	deadline := time.Now().Add(time.Second)
	for m.InFlight("a") != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := m.InFlight("a"); got != 0 {
		t.Fatalf("in-flight should drop to 0 after stream close, got %d", got)
	}
}

func TestWrapInFlightStreamReleasesWhenClientGoesAway(t *testing.T) {
	m := &Manager{}
	release := m.beginInFlight("a")
	ctx, cancel := context.WithCancel(context.Background())
	src := make(chan cliproxyexecutor.StreamChunk) // upstream never closes
	wrapInFlightStream(ctx, &cliproxyexecutor.StreamResult{Chunks: src}, release)
	cancel()
	deadline := time.Now().Add(time.Second)
	for m.InFlight("a") != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := m.InFlight("a"); got != 0 {
		t.Fatalf("in-flight should drop to 0 after client context ends, got %d", got)
	}
}
