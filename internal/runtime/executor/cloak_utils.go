package executor

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// userIDPattern matches Claude Code format: user_[64-hex]_account__session_[uuid-v4]
var userIDPattern = regexp.MustCompile(`^user_[a-fA-F0-9]{64}_account__session_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// generateFakeUserID generates a fake user ID in Claude Code format.
// Format: user_[64-hex-chars]_account__session_[UUID-v4]
// Deprecated: Use session pool via getPooledUserID for consistent sessions.
func generateFakeUserID() string {
	hexBytes := make([]byte, 32)
	_, _ = rand.Read(hexBytes)
	hexPart := hex.EncodeToString(hexBytes)
	uuidPart := uuid.New().String()
	return "user_" + hexPart + "_account__session_" + uuidPart
}

// getPooledUserID returns a user ID from the session pool.
// This ensures consistent session UUIDs across requests while rotating periodically.
// Parameters:
//   - authID: Unique identifier for the auth credential
//   - apiKey: The API key used for consistent hashing
//   - clientUserID: The user_id from the client request (optional, for hash extraction)
//   - maxSessions: Maximum sessions per pool (0 = default)
//   - rotationInterval: Time between rotations (0 = default)
func getPooledUserID(authID, apiKey, clientUserID string, maxSessions int, rotationInterval time.Duration) string {
	pool := GetGlobalSessionPool()
	return pool.GetUserID(authID, apiKey, clientUserID, maxSessions, rotationInterval)
}

// isValidUserID checks if a user ID matches Claude Code format.
func isValidUserID(userID string) bool {
	return userIDPattern.MatchString(userID)
}

// shouldCloak determines if request should be cloaked based on config and client User-Agent.
// Returns true if cloaking should be applied.
func shouldCloak(cloakMode string, userAgent string) bool {
	switch strings.ToLower(cloakMode) {
	case "always":
		return true
	case "never":
		return false
	default: // "auto" or empty
		// If client is Claude Code, don't cloak
		return !strings.HasPrefix(userAgent, "claude-cli")
	}
}

// isClaudeCodeClient checks if the User-Agent indicates a Claude Code client.
func isClaudeCodeClient(userAgent string) bool {
	return strings.HasPrefix(userAgent, "claude-cli")
}
