package access

import (
	"context"
	"net/http"
	"sync"
)

// Manager coordinates authentication providers.
type Manager struct {
	mu             sync.RWMutex
	providers      []Provider
	allowAnonymous bool
}

// NewManager constructs an empty manager.
func NewManager() *Manager {
	return &Manager{}
}

// SetAllowAnonymous controls whether the data plane fails open when no access
// providers are configured. When false (the secure default), a request that
// reaches a manager with zero providers is rejected with a missing-credentials
// error; when true, such a request is allowed through (open proxy).
func (m *Manager) SetAllowAnonymous(allow bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.allowAnonymous = allow
	m.mu.Unlock()
}

// SetProviders replaces the active provider list.
func (m *Manager) SetProviders(providers []Provider) {
	if m == nil {
		return
	}
	cloned := make([]Provider, len(providers))
	copy(cloned, providers)
	m.mu.Lock()
	m.providers = cloned
	m.mu.Unlock()
}

// Providers returns a snapshot of the active providers.
func (m *Manager) Providers() []Provider {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	snapshot := make([]Provider, len(m.providers))
	copy(snapshot, m.providers)
	return snapshot
}

// Authenticate evaluates providers until one succeeds.
func (m *Manager) Authenticate(ctx context.Context, r *http.Request) (*Result, *AuthError) {
	if m == nil {
		return nil, nil
	}
	providers := m.Providers()
	if len(providers) == 0 {
		m.mu.RLock()
		allowAnonymous := m.allowAnonymous
		m.mu.RUnlock()
		if allowAnonymous {
			return nil, nil
		}
		// Fail closed: no access providers configured means no way to
		// authenticate, so reject rather than silently running an open proxy.
		return nil, NewNoCredentialsError()
	}

	var (
		missing bool
		invalid bool
	)

	for _, provider := range providers {
		if provider == nil {
			continue
		}
		res, authErr := provider.Authenticate(ctx, r)
		if authErr == nil {
			return res, nil
		}
		if IsAuthErrorCode(authErr, AuthErrorCodeNotHandled) {
			continue
		}
		if IsAuthErrorCode(authErr, AuthErrorCodeNoCredentials) {
			missing = true
			continue
		}
		if IsAuthErrorCode(authErr, AuthErrorCodeInvalidCredential) {
			invalid = true
			continue
		}
		return nil, authErr
	}

	if invalid {
		return nil, NewInvalidCredentialError()
	}
	if missing {
		return nil, NewNoCredentialsError()
	}
	return nil, NewNoCredentialsError()
}
