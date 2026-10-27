package auth

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// supportsSelectionSnapshots includes every fallback that can receive candidates.
// Custom selectors retain the public contract of receiving complete Auth snapshots,
// including when a plugin declines to handle the pick.
func supportsSelectionSnapshots(selector Selector) bool {
	switch s := selector.(type) {
	case *RoundRobinSelector:
		return s != nil
	case *WeightedRoundRobinSelector:
		return s != nil
	case *FillFirstSelector:
		return s != nil
	case *SessionAffinitySelector:
		return s != nil && supportsSelectionSnapshots(s.fallback)
	default:
		return false
	}
}

// snapshotAuthsForSelection must run under m.mu after availability filtering.
// Built-in selectors use prevalidatedAuthCandidatesKey to avoid reading model,
// quota and token state again. Attributes are also needed by plugin projections;
// the only metadata consumed by the built-ins is weight and websocket capability.
// Every map that escapes the lock is owned by this snapshot.
func snapshotAuthsForSelection(selector Selector, auths []*Auth) []*Auth {
	if !supportsSelectionSnapshots(selector) {
		return cloneAuthSlice(auths)
	}
	snapshots := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		snapshot := &Auth{
			ID:                auth.ID,
			RegistrationEpoch: auth.RegistrationEpoch,
			Generation:        auth.Generation,
			Provider:          auth.Provider,
			Label:             auth.Label,
			Status:            auth.Status,
			Attributes:        maps.Clone(auth.Attributes),
		}
		_, hasWeight := auth.Metadata[AttributeWeight]
		_, hasWebsockets := auth.Metadata["websockets"]
		if hasWeight || hasWebsockets {
			// Normalize to scalars instead of sharing arbitrary metadata values.
			snapshot.Metadata = make(map[string]any, 2)
			if hasWeight {
				snapshot.Metadata[AttributeWeight] = authWeight(auth)
			}
			if hasWebsockets {
				snapshot.Metadata["websockets"] = authWebsocketsEnabled(auth)
			}
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots
}

// resolveSelectionSnapshot copies only the selected credential's complete state.
// Selection runs outside m.mu, so the credential may have been refreshed, removed,
// re-registered or blocked in the meantime. Never execute a projection or a stale
// registration, and obtain the matching executor in the same critical section.
func (m *Manager) resolveSelectionSnapshot(ctx context.Context, selected *Auth, model string, opts cliproxyexecutor.Options) (*Auth, ProviderExecutor, error) {
	if m.codexPools.Load() == nil {
		return m.resolveLegacySelectionSnapshot(ctx, selected, model, opts)
	}
	guard, errLock := m.lockAuthRead(ctx, selected.ID)
	if errLock != nil {
		return nil, nil, &Error{Code: "auth_unavailable", Message: staleSelectionMessage, Retryable: true, HTTPStatus: http.StatusServiceUnavailable}
	}
	current := guard.auths[selected.ID]
	if current != nil && !current.indexAssigned && guard.pool == nil {
		guard.unlock()
		guard, errLock = m.lockAuth(ctx, selected.ID)
		if errLock != nil {
			return nil, nil, &Error{Code: "auth_unavailable", Message: staleSelectionMessage, Retryable: true, HTTPStatus: http.StatusServiceUnavailable}
		}
		current = guard.auths[selected.ID]
	}
	defer guard.unlock()
	if current != nil && !current.indexAssigned {
		current.EnsureIndex()
	}
	staleSelection := func() (*Auth, ProviderExecutor, error) {
		return nil, nil, &Error{
			Code: "auth_unavailable", Message: staleSelectionMessage,
			Retryable: true, HTTPStatus: http.StatusServiceUnavailable,
		}
	}
	if current == nil || current.RegistrationEpoch != selected.RegistrationEpoch ||
		executorKeyFromAuth(current) != executorKeyFromAuth(selected) ||
		!authSelectionEligibilityForRequest(ctx, opts).allows(current) {
		return staleSelection()
	}
	if pinned := pinnedAuthIDFromMetadata(opts.Metadata); pinned != "" && current.ID != pinned {
		return staleSelection()
	}
	if blocked, _, _ := isAuthBlockedForModel(current, m.selectionModelForAuth(current, model), time.Now()); blocked {
		return staleSelection()
	}
	if strings.TrimSpace(model) != "" && !m.authSupportsRouteModel(registry.GetGlobalRegistry(), current, model) {
		return staleSelection()
	}
	var executor ProviderExecutor
	if guard.pool != nil {
		executor = m.shared.Load().executors[executorKeyFromAuth(current)]
	} else {
		executor = m.executors[executorKeyFromAuth(current)]
	}
	if executor == nil {
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	return current.Clone(), executor, nil
}

const (
	staleSelectionMessage = "selected auth changed; retry selection"
	// maxStaleSelectionRetries bounds re-picks when the selected credential changed between
	// the lock-free selection and resolving it, e.g. it entered cooldown meanwhile.
	maxStaleSelectionRetries = 3
)

type staleSelectionRetriesKey struct{}

// staleSelectionRetry reports whether a stale selection should be retried and returns the
// context and tried set for the next pick. The caller's tried map is never modified.
func staleSelectionRetry(ctx context.Context, err error, tried map[string]struct{}, authID string) (context.Context, map[string]struct{}, bool) {
	var authErr *Error
	if err == nil || !errors.As(err, &authErr) || authErr == nil || authErr.Message != staleSelectionMessage {
		return ctx, tried, false
	}
	retries, _ := ctx.Value(staleSelectionRetriesKey{}).(int)
	if retries >= maxStaleSelectionRetries {
		return ctx, tried, false
	}
	next := make(map[string]struct{}, len(tried)+1)
	for id := range tried {
		next[id] = struct{}{}
	}
	next[authID] = struct{}{}
	return context.WithValue(ctx, staleSelectionRetriesKey{}, retries+1), next, true
}

func (m *Manager) resolveLegacySelectionSnapshot(ctx context.Context, selected *Auth, model string, opts cliproxyexecutor.Options) (*Auth, ProviderExecutor, error) {
	m.mu.RLock()
	current := m.auths[selected.ID]
	if current != nil && !current.indexAssigned {
		m.mu.RUnlock()
		m.mu.Lock()
		defer m.mu.Unlock()
		current = m.auths[selected.ID]
		if current != nil {
			current.EnsureIndex()
		}
	} else {
		defer m.mu.RUnlock()
	}
	staleSelection := func() (*Auth, ProviderExecutor, error) {
		return nil, nil, &Error{
			Code: "auth_unavailable", Message: staleSelectionMessage,
			Retryable: true, HTTPStatus: http.StatusServiceUnavailable,
		}
	}
	if current == nil || current.RegistrationEpoch != selected.RegistrationEpoch ||
		executorKeyFromAuth(current) != executorKeyFromAuth(selected) ||
		!authSelectionEligibilityForRequest(ctx, opts).allows(current) {
		return staleSelection()
	}
	if pinned := pinnedAuthIDFromMetadata(opts.Metadata); pinned != "" && current.ID != pinned {
		return staleSelection()
	}
	if blocked, _, _ := isAuthBlockedForModel(current, m.selectionModelForAuth(current, model), time.Now()); blocked {
		return staleSelection()
	}
	if strings.TrimSpace(model) != "" && !m.authSupportsRouteModel(registry.GetGlobalRegistry(), current, model) {
		return staleSelection()
	}
	executor := m.executors[executorKeyFromAuth(current)]
	if executor == nil {
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	return current.Clone(), executor, nil
}
