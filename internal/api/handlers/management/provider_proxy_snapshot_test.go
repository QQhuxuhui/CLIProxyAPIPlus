package management

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestProviderProxyUpdatesPreserveRuntimeSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name, method, query, body string
		want                      map[string]string
	}{
		{"put", "PUT", "", `{"value":{"codex":"direct"}}`, map[string]string{"codex": "direct"}},
		{"patch", "PATCH", "", `{"value":{"codex":"direct"}}`, map[string]string{"codex": "direct", "claude": "http://old:8080"}},
		{"delete one", "DELETE", "?provider=codex", "", map[string]string{"claude": "http://old:8080"}},
		{"delete all", "DELETE", "", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{SDKConfig: config.SDKConfig{ProviderProxyURLs: map[string]string{"codex": "http://old:8080", "claude": "http://old:8080"}}}
			original := cfg.CloneForRuntime()
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("port: 8317\n"), 0600); err != nil {
				t.Fatal(err)
			}
			h := &Handler{cfg: cfg, configFilePath: path}
			reloads, done := captureConfigReload(h)
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for i := 0; i < 100000; i++ {
					_ = cfg.ResolveProxyURL("", "codex")
				}
			}()
			close(start)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(tc.method, "/provider-proxy-url"+tc.query, strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			switch tc.method {
			case "PUT":
				h.PutProviderProxyURLs(c)
			case "PATCH":
				h.PatchProviderProxyURLs(c)
			default:
				h.DeleteProviderProxyURLs(c)
			}
			wg.Wait()
			if w.Code != 200 {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			reloaded := waitForAsyncReload(t, reloads)
			waitForReloadDone(t, done)
			if !reflect.DeepEqual(cfg.ProviderProxyURLs, original.ProviderProxyURLs) {
				t.Error("update mutated the active runtime snapshot")
			}
			if !reflect.DeepEqual(reloaded.ProviderProxyURLs, tc.want) {
				t.Fatalf("reloaded proxies = %v, want %v", reloaded.ProviderProxyURLs, tc.want)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := config.ParseConfigBytes(data)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(persisted.ProviderProxyURLs, tc.want) {
				t.Fatalf("persisted proxies = %v, want %v", persisted.ProviderProxyURLs, tc.want)
			}
		})
	}
}

func TestProviderProxySaveFailurePreservesConfig(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProviderProxyURLs: map[string]string{"codex": "direct"}}}
	h := &Handler{cfg: cfg, configFilePath: filepath.Join(t.TempDir(), "missing", "config.yaml")}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("PUT", "/", strings.NewReader(`{"value":{"codex":"http://new:8080"}}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PutProviderProxyURLs(c)
	if w.Code != 500 {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if h.cfg != cfg || cfg.ProviderProxyURL("codex") != "direct" {
		t.Fatal("failed save changed active config")
	}
}
