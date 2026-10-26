package auth

import (
	"context"
	"testing"
)

// blockingSaveStore blocks every Save until release is closed.
type blockingSaveStore struct {
	entered chan string
	release chan struct{}
}

func (s *blockingSaveStore) List(context.Context) ([]*Auth, error) { return nil, nil }
func (s *blockingSaveStore) Delete(context.Context, string) error  { return nil }
func (s *blockingSaveStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.entered <- auth.ID
	<-s.release
	return auth.ID, nil
}

func TestMarkResultPersistsOutsideManagerLock(t *testing.T) {
	store := &blockingSaveStore{entered: make(chan string, 4), release: make(chan struct{})}
	m := NewManager(store, nil, nil)
	m.auths["a"] = &Auth{ID: "a", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "t"}}

	done := make(chan struct{})
	go func() {
		m.MarkResult(context.Background(), Result{AuthID: "a", Provider: "codex", Model: "gpt-6-luna", Success: true})
		close(done)
	}()
	if id := <-store.entered; id != "a" {
		t.Fatalf("saved %q, want a", id)
	}
	// A slow disk write must not hold the manager lock that every other request needs.
	if !m.mu.TryLock() {
		close(store.release)
		t.Fatal("manager lock held while persisting")
	}
	if got := m.auths["a"].Success; got != 1 {
		m.mu.Unlock()
		close(store.release)
		t.Fatalf("in-memory success = %d, want 1 before persist finishes", got)
	}
	m.mu.Unlock()
	close(store.release)
	<-done
}

func TestRecordAvailabilityNeutralResultPersistsOutsideManagerLock(t *testing.T) {
	store := &blockingSaveStore{entered: make(chan string, 4), release: make(chan struct{})}
	m := NewManager(store, nil, nil)
	m.auths["a"] = &Auth{ID: "a", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "t"}}

	done := make(chan struct{})
	go func() {
		m.recordAvailabilityNeutralResult(context.Background(), Result{AuthID: "a", Provider: "codex", Success: true})
		close(done)
	}()
	<-store.entered
	if !m.mu.TryLock() {
		close(store.release)
		t.Fatal("manager lock held while persisting")
	}
	m.mu.Unlock()
	close(store.release)
	<-done
}
