package codex

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSaveTokenToFile_WritesPrivateMode asserts that persisted credential files are
// created without group/world read access (mode & 0o077 == 0), closing the S16-adjacent
// gap where os.Create left OAuth token files at 0o644. Representative for the identical
// os.OpenFile(0o600) change applied to the claude/xai/kimi/vertex token savers too.
func TestSaveTokenToFile_WritesPrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex-token.json")

	ts := &CodexTokenStorage{}
	if err := ts.SaveTokenToFile(path); err != nil {
		t.Fatalf("SaveTokenToFile: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("token file mode = %o, want no group/other bits (0o600); got group/other access", perm)
	}
}
