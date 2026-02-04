package executor

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	// DefaultMaxSessions is the default number of sessions per auth pool.
	DefaultMaxSessions = 5
	// DefaultRotationInterval is the default time between session rotations.
	DefaultRotationInterval = 6 * time.Hour
	// DefaultGracePeriod is the overlap time during session rotation.
	DefaultGracePeriod = 5 * time.Minute
)

// SessionEntry represents a single session with lifecycle metadata.
type SessionEntry struct {
	UUID      string    // Session UUID (v4)
	CreatedAt time.Time // When the session was created
	ActiveAt  time.Time // When the session becomes active (for soft rotation)
	RetireAt  time.Time // When the session should retire (zero = active indefinitely)
}

// IsActive returns true if the session is currently active.
func (s *SessionEntry) IsActive(now time.Time) bool {
	// Session is active if:
	// 1. ActiveAt is zero or <= now (session has started)
	// 2. RetireAt is zero or > now (session hasn't retired)
	started := s.ActiveAt.IsZero() || !now.Before(s.ActiveAt)
	notRetired := s.RetireAt.IsZero() || now.Before(s.RetireAt)
	return started && notRetired
}

// AuthSessionPool manages sessions for a single authentication credential.
type AuthSessionPool struct {
	authID           string           // Identifier for this auth (e.g., API key hash)
	hashPart         string           // The 64-hex hash part for user_id
	hashSource       string           // "client" or "channel"
	sessions         []SessionEntry   // Current sessions
	maxSessions      int              // Maximum concurrent sessions
	rotationInterval time.Duration    // Time between rotations
	lastRotation     time.Time        // Last rotation timestamp
	mu               sync.RWMutex
}

// NewAuthSessionPool creates a new session pool for an auth credential.
func NewAuthSessionPool(authID, hashPart, hashSource string, maxSessions int, rotationInterval time.Duration) *AuthSessionPool {
	if maxSessions <= 0 {
		maxSessions = DefaultMaxSessions
	}
	if rotationInterval <= 0 {
		rotationInterval = DefaultRotationInterval
	}

	pool := &AuthSessionPool{
		authID:           authID,
		hashPart:         hashPart,
		hashSource:       hashSource,
		sessions:         make([]SessionEntry, 0, maxSessions),
		maxSessions:      maxSessions,
		rotationInterval: rotationInterval,
		lastRotation:     time.Now(),
	}

	// Initialize with one session
	pool.addSession(time.Now())

	return pool
}

// GetUserID returns a user_id for the given API key, using consistent hashing.
func (p *AuthSessionPool) GetUserID(apiKey string, now time.Time) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Check if rotation is needed
	p.maybeRotate(now)

	// Get active sessions
	active := p.getActiveSessions(now)
	if len(active) == 0 {
		// No active sessions, create one
		p.addSession(now)
		active = p.getActiveSessions(now)
	}

	// Select session using consistent hashing
	sessionUUID := p.selectSessionByKey(apiKey, active)

	// Build user_id: user_[64-hex]_account__session_[uuid]
	return "user_" + p.hashPart + "_account__session_" + sessionUUID
}

// getActiveSessions returns currently active sessions.
func (p *AuthSessionPool) getActiveSessions(now time.Time) []string {
	result := make([]string, 0, len(p.sessions))
	for i := range p.sessions {
		if p.sessions[i].IsActive(now) {
			result = append(result, p.sessions[i].UUID)
		}
	}
	return result
}

// selectSessionByKey uses consistent hashing to select a session.
func (p *AuthSessionPool) selectSessionByKey(apiKey string, sessions []string) string {
	if len(sessions) == 0 {
		return uuid.New().String()
	}
	if len(sessions) == 1 {
		return sessions[0]
	}

	// Hash the API key to get a consistent index
	h := sha256.Sum256([]byte(apiKey))
	idx := binary.BigEndian.Uint64(h[:8]) % uint64(len(sessions))
	return sessions[idx]
}

// maybeRotate checks if rotation is due and performs soft rotation.
func (p *AuthSessionPool) maybeRotate(now time.Time) {
	if now.Sub(p.lastRotation) < p.rotationInterval {
		return
	}

	// Clean up retired sessions
	p.cleanupRetired(now)

	// Only rotate if we have sessions and haven't reached max
	activeCount := len(p.getActiveSessions(now))
	if activeCount >= p.maxSessions {
		// Mark oldest for retirement
		p.retireOldest(now)
	}

	// Add new session
	p.addSession(now)
	p.lastRotation = now
}

// addSession adds a new session to the pool.
func (p *AuthSessionPool) addSession(now time.Time) {
	entry := SessionEntry{
		UUID:      uuid.New().String(),
		CreatedAt: now,
		ActiveAt:  now, // Immediately active
	}
	p.sessions = append(p.sessions, entry)
}

// retireOldest marks the oldest active session for retirement.
func (p *AuthSessionPool) retireOldest(now time.Time) {
	var oldestIdx = -1
	var oldestTime time.Time

	for i := range p.sessions {
		if !p.sessions[i].IsActive(now) {
			continue
		}
		if oldestIdx < 0 || p.sessions[i].CreatedAt.Before(oldestTime) {
			oldestIdx = i
			oldestTime = p.sessions[i].CreatedAt
		}
	}

	if oldestIdx >= 0 {
		p.sessions[oldestIdx].RetireAt = now.Add(DefaultGracePeriod)
	}
}

// cleanupRetired removes sessions that have passed their retirement time.
func (p *AuthSessionPool) cleanupRetired(now time.Time) {
	cleaned := make([]SessionEntry, 0, len(p.sessions))
	for i := range p.sessions {
		// Keep if not yet retired
		if p.sessions[i].RetireAt.IsZero() || now.Before(p.sessions[i].RetireAt) {
			cleaned = append(cleaned, p.sessions[i])
		}
	}
	p.sessions = cleaned
}

// SessionPoolManager manages session pools for all auth credentials.
type SessionPoolManager struct {
	pools            map[string]*AuthSessionPool // key = auth ID
	defaultMax       int
	rotationInterval time.Duration
	mu               sync.RWMutex
}

var (
	globalSessionPool     *SessionPoolManager
	globalSessionPoolOnce sync.Once
)

// GetGlobalSessionPool returns the singleton session pool manager.
func GetGlobalSessionPool() *SessionPoolManager {
	globalSessionPoolOnce.Do(func() {
		globalSessionPool = NewSessionPoolManager(DefaultMaxSessions, DefaultRotationInterval)
	})
	return globalSessionPool
}

// NewSessionPoolManager creates a new session pool manager.
func NewSessionPoolManager(defaultMax int, rotationInterval time.Duration) *SessionPoolManager {
	if defaultMax <= 0 {
		defaultMax = DefaultMaxSessions
	}
	if rotationInterval <= 0 {
		rotationInterval = DefaultRotationInterval
	}

	return &SessionPoolManager{
		pools:            make(map[string]*AuthSessionPool),
		defaultMax:       defaultMax,
		rotationInterval: rotationInterval,
	}
}

// GetUserID returns a user_id for the given auth and API key.
// It uses the client's existing user_id hash if valid, otherwise generates from API key.
func (m *SessionPoolManager) GetUserID(authID, apiKey, clientUserID string, maxSessions int, rotationInterval time.Duration) string {
	m.mu.Lock()

	// Determine hash part
	hashPart, hashSource := extractOrGenerateHash(clientUserID, apiKey)

	// Create pool key from auth ID + hash part for uniqueness
	poolKey := authID + ":" + hashPart

	pool, exists := m.pools[poolKey]
	if !exists {
		if maxSessions <= 0 {
			maxSessions = m.defaultMax
		}
		if rotationInterval <= 0 {
			rotationInterval = m.rotationInterval
		}
		pool = NewAuthSessionPool(authID, hashPart, hashSource, maxSessions, rotationInterval)
		m.pools[poolKey] = pool
	}
	m.mu.Unlock()

	return pool.GetUserID(apiKey, time.Now())
}

// extractOrGenerateHash extracts hash from client user_id or generates from API key.
func extractOrGenerateHash(clientUserID, apiKey string) (hashPart, source string) {
	// Priority 1: Extract from client's user_id
	if hash, ok := extractHashFromUserID(clientUserID); ok {
		return hash, "client"
	}

	// Priority 2: Generate from API key (channel hash)
	return generateChannelHash(apiKey), "channel"
}

// extractHashFromUserID extracts the 64-hex hash part from a user_id.
// Format: user_[64-hex]_account__session_[uuid]
func extractHashFromUserID(userID string) (string, bool) {
	if !strings.HasPrefix(userID, "user_") {
		return "", false
	}

	parts := strings.Split(userID, "_account__session_")
	if len(parts) != 2 {
		return "", false
	}

	hashPart := strings.TrimPrefix(parts[0], "user_")
	if len(hashPart) != 64 || !isHexString(hashPart) {
		return "", false
	}

	return hashPart, true
}

// generateChannelHash generates a 64-char hex hash from an API key.
func generateChannelHash(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:]) // 64 hex chars
}

// isHexString checks if a string contains only hex characters.
func isHexString(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// selectWeightedSession selects a session with weights favoring earlier sessions.
// This is an alternative to consistent hashing for load balancing.
func selectWeightedSession(sessions []string) string {
	n := len(sessions)
	if n == 0 {
		return ""
	}
	if n == 1 {
		return sessions[0]
	}

	// Total weight: N + (N-1) + ... + 1 = N*(N+1)/2
	totalWeight := n * (n + 1) / 2
	pick := cryptoRandIntn(totalWeight)

	cumulative := 0
	for i := 0; i < n; i++ {
		cumulative += (n - i) // Weight decreases for later sessions
		if pick < cumulative {
			return sessions[i]
		}
	}
	return sessions[n-1]
}

// cryptoRandIntn returns a cryptographically random int in [0, n).
func cryptoRandIntn(n int) int {
	if n <= 0 {
		return 0
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return int(binary.BigEndian.Uint64(b[:]) % uint64(n))
}
