package managementasset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	logtest "github.com/sirupsen/logrus/hooks/test"
)

// newReleaseServer returns an httptest server that serves a GitHub-style release
// response referencing the given download URL and digest for the management asset.
func newReleaseServer(t *testing.T, downloadURL, digest string) *httptest.Server {
	t.Helper()
	body, err := json.Marshal(releaseResponse{Assets: []releaseAsset{{
		Name:               managementAssetName,
		BrowserDownloadURL: downloadURL,
		Digest:             digest,
	}}})
	if err != nil {
		t.Fatalf("marshal release response: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSyncManagementAsset_FallbackDisabledByDefault (S02) asserts that when the
// verified GitHub release path fails and no local copy exists, the unverified
// fallback origin is NOT contacted and no file is written unless opted in.
func TestSyncManagementAsset_FallbackDisabledByDefault(t *testing.T) {
	// Primary release fetch fails (500).
	releaseSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(releaseSrv.Close)

	var fallbackHits atomic.Int32
	fallbackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackHits.Add(1)
		_, _ = w.Write([]byte("<html>fallback panel</html>"))
	}))
	t.Cleanup(fallbackSrv.Close)

	localPath := filepath.Join(t.TempDir(), managementAssetName)

	syncManagementAsset(context.Background(), http.DefaultClient, releaseSrv.URL, fallbackSrv.URL, localPath, true, false)

	if got := fallbackHits.Load(); got != 0 {
		t.Fatalf("fallback URL was fetched %d time(s); expected 0 when allowUnverifiedFallback is false", got)
	}
	if _, err := os.Stat(localPath); !os.IsNotExist(err) {
		t.Fatalf("management asset was written to %s; expected no file when fallback is disabled (stat err=%v)", localPath, err)
	}
}

// TestSyncManagementAsset_FallbackAllowedWhenOptedIn (S02) asserts that opting
// in restores the previous behavior: the fallback is fetched and written.
func TestSyncManagementAsset_FallbackAllowedWhenOptedIn(t *testing.T) {
	releaseSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(releaseSrv.Close)

	const fallbackBody = "<html>fallback panel</html>"
	var fallbackHits atomic.Int32
	fallbackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackHits.Add(1)
		_, _ = w.Write([]byte(fallbackBody))
	}))
	t.Cleanup(fallbackSrv.Close)

	localPath := filepath.Join(t.TempDir(), managementAssetName)

	syncManagementAsset(context.Background(), http.DefaultClient, releaseSrv.URL, fallbackSrv.URL, localPath, true, true)

	if got := fallbackHits.Load(); got != 1 {
		t.Fatalf("fallback URL was fetched %d time(s); expected 1 when allowUnverifiedFallback is true", got)
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("expected management asset written from fallback: %v", err)
	}
	if string(data) != fallbackBody {
		t.Fatalf("management asset content = %q, want %q", string(data), fallbackBody)
	}
}

// TestSyncManagementAsset_EmptyDigestWarnsButInstalls (S10) asserts that when the
// GitHub release provides no digest, the asset is still installed from the verified
// primary path but a visible warning is emitted (no longer silent).
func TestSyncManagementAsset_EmptyDigestWarnsButInstalls(t *testing.T) {
	const assetBody = "<html>primary panel</html>"
	downloadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(assetBody))
	}))
	t.Cleanup(downloadSrv.Close)

	releaseSrv := newReleaseServer(t, downloadSrv.URL, "") // empty digest

	hook := logtest.NewGlobal()
	defer hook.Reset()

	localPath := filepath.Join(t.TempDir(), managementAssetName)
	syncManagementAsset(context.Background(), http.DefaultClient, releaseSrv.URL, "", localPath, true, false)

	data, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("expected asset installed from primary path even without digest: %v", err)
	}
	if string(data) != assetBody {
		t.Fatalf("management asset content = %q, want %q", string(data), assetBody)
	}
	if !hasWarn(hook, "WITHOUT digest verification") {
		t.Fatalf("expected a warning about missing digest verification; got entries: %v", entryMessages(hook))
	}
}

// TestSyncManagementAsset_MatchingDigestNoWarn (S10) asserts that when a digest is
// present and matches, the asset installs with no missing-digest warning.
func TestSyncManagementAsset_MatchingDigestNoWarn(t *testing.T) {
	const assetBody = "<html>primary panel</html>"
	sum := sha256.Sum256([]byte(assetBody))
	digest := "sha256:" + hex.EncodeToString(sum[:])

	downloadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(assetBody))
	}))
	t.Cleanup(downloadSrv.Close)

	releaseSrv := newReleaseServer(t, downloadSrv.URL, digest)

	hook := logtest.NewGlobal()
	defer hook.Reset()

	localPath := filepath.Join(t.TempDir(), managementAssetName)
	syncManagementAsset(context.Background(), http.DefaultClient, releaseSrv.URL, "", localPath, true, false)

	if _, err := os.Stat(localPath); err != nil {
		t.Fatalf("expected asset installed when digest matches: %v", err)
	}
	if hasWarn(hook, "WITHOUT digest verification") {
		t.Fatalf("did not expect a missing-digest warning when digest matches; got entries: %v", entryMessages(hook))
	}
}

// TestSyncManagementAsset_DigestMismatchAborts (S10 guard) asserts that when the
// release advertises a non-empty digest that does NOT match the downloaded bytes,
// the asset is rejected and never written to disk. This pins the tamper-detection
// behavior so a future refactor cannot silently drop it.
func TestSyncManagementAsset_DigestMismatchAborts(t *testing.T) {
	const assetBody = "<html>primary panel</html>"
	downloadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(assetBody))
	}))
	t.Cleanup(downloadSrv.Close)

	// Digest for DIFFERENT content, so it cannot match assetBody.
	sum := sha256.Sum256([]byte("something else entirely"))
	wrongDigest := "sha256:" + hex.EncodeToString(sum[:])
	releaseSrv := newReleaseServer(t, downloadSrv.URL, wrongDigest)

	localPath := filepath.Join(t.TempDir(), managementAssetName)
	syncManagementAsset(context.Background(), http.DefaultClient, releaseSrv.URL, "", localPath, true, false)

	if _, err := os.Stat(localPath); !os.IsNotExist(err) {
		t.Fatalf("asset was written despite a digest mismatch (stat err=%v); tamper-detection abort was bypassed", err)
	}
}

func hasWarn(hook *logtest.Hook, substr string) bool {
	for _, e := range hook.AllEntries() {
		if strings.Contains(e.Message, substr) {
			return true
		}
	}
	return false
}

func entryMessages(hook *logtest.Hook) []string {
	msgs := make([]string, 0)
	for _, e := range hook.AllEntries() {
		msgs = append(msgs, e.Message)
	}
	return msgs
}
