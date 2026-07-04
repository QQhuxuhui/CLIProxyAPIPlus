package cliproxy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// TestEnsureAuthDir_CreatesPrivateDirectory verifies that ensureAuthDir creates
// the auth directory without any group/other permission bits, matching the 0700
// hardening used everywhere else that credential material is stored. The check is
// umask-robust: it asserts the absence of group/other bits rather than an exact
// 0700 match.
func TestEnsureAuthDir_CreatesPrivateDirectory(t *testing.T) {
	authDir := filepath.Join(t.TempDir(), "auths")
	service := &Service{cfg: &config.Config{AuthDir: authDir}}

	if err := service.ensureAuthDir(); err != nil {
		t.Fatalf("ensureAuthDir: %v", err)
	}

	info, err := os.Stat(authDir)
	if err != nil {
		t.Fatalf("stat auth dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected a directory at %s", authDir)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Fatalf("auth dir perms = %#o, want no group/other access (mode & 0o077 == 0)", mode)
	}
}
