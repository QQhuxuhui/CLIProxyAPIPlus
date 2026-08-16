package webimage

import (
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const sessionTTL = 25 * time.Minute

// Identity contains stable browser identifiers derived from one auth ID.
type Identity struct {
	DeviceID  string
	SessionID string
}

// Session owns one account-isolated browser transport and cookie jar.
type Session struct {
	Client        *http.Client
	Identity      Identity
	UserAgent     string
	ClientVersion string

	signature string
	createdAt time.Time
}

// SessionPool caches one browser session per selected Codex auth ID.
type SessionPool struct {
	cfg *config.Config

	mu       sync.Mutex
	sessions map[string]*Session
}

func NewSessionPool(cfg *config.Config) *SessionPool {
	return &SessionPool{cfg: cfg, sessions: make(map[string]*Session)}
}

// Get returns an isolated browser session for credentials.AuthID.
func (p *SessionPool) Get(credentials Credentials) (*Session, error) {
	return p.get(credentials, time.Now())
}

func (p *SessionPool) get(credentials Credentials, now time.Time) (*Session, error) {
	authID := credentials.AuthID
	signature := sessionSignature(p.cfg, credentials)

	p.mu.Lock()
	defer p.mu.Unlock()
	if current := p.sessions[authID]; current != nil && current.signature == signature && now.Sub(current.createdAt) < sessionTTL {
		return current, nil
	}

	session, errBuild := buildSession(p.cfg, credentials, signature, now)
	if errBuild != nil {
		return nil, errBuild
	}
	if previous := p.sessions[authID]; previous != nil && previous.Client != nil {
		previous.Client.CloseIdleConnections()
	}
	p.sessions[authID] = session
	return session, nil
}

// Close releases all cached browser transports.
func (p *SessionPool) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for authID, session := range p.sessions {
		if session != nil && session.Client != nil {
			session.Client.CloseIdleConnections()
		}
		delete(p.sessions, authID)
	}
}
