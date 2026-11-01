package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Runtime state lives in hidden subdirectories of the auth dir and must never load as an account.
func writeAuthDirWithHiddenState(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"codex-root.json": `{"type":"codex"}`,
		filepath.Join(".codex-account-concurrency", "limits.json"): `{"version":1,"limits":{"codex-root.json":8}}`,
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func requireOnlyRootAccount(t *testing.T, auths []*cliproxyauth.Auth) {
	t.Helper()
	if len(auths) != 1 || auths[0].ID != "codex-root.json" {
		ids := make([]string, 0, len(auths))
		for _, auth := range auths {
			ids = append(ids, auth.ID)
		}
		t.Fatalf("List() ids = %v", ids)
	}
}

func TestObjectTokenStoreListSkipsHiddenSubdirectories(t *testing.T) {
	// The root is hidden on purpose: only subdirectories are excluded.
	dir := filepath.Join(t.TempDir(), ".cli-proxy-api")
	writeAuthDirWithHiddenState(t, dir)
	store := &ObjectTokenStore{authDir: dir}
	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	requireOnlyRootAccount(t, auths)
}

func TestGitTokenStoreListSkipsHiddenSubdirectories(t *testing.T) {
	root := t.TempDir()
	remoteDir := setupGitRemoteRepository(t, root, "master", testBranchSpec{name: "master", contents: "remote default branch\n"})
	dir := filepath.Join(root, "workspace", "auths")
	store := NewGitTokenStore(remoteDir, "", "", "")
	store.SetBaseDir(dir)
	if err := store.EnsureRepository(); err != nil {
		t.Fatal(err)
	}
	writeAuthDirWithHiddenState(t, dir)
	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	requireOnlyRootAccount(t, auths)
}
