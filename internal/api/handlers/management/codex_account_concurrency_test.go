package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type concurrencyFixture struct {
	h       *Handler
	manager *coreauth.Manager
	authDir string
}

func newManagementConcurrencyFixture(t *testing.T, names ...string) *concurrencyFixture {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	cfg := &config.Config{AuthDir: authDir}
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	manager.SetConfig(cfg)
	for _, name := range names {
		path := filepath.Join(authDir, name)
		if err := os.WriteFile(path, []byte(`{"type":"codex","access_token":"t"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		record := &coreauth.Auth{
			ID: name, FileName: name, Provider: "codex", Status: coreauth.StatusActive,
			Attributes: map[string]string{"path": path, coreauth.AttributeAuthKind: coreauth.AuthKindOAuth},
			Metadata:   map[string]any{"type": "codex", "access_token": "t"},
		}
		if _, err := manager.Register(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	h := NewHandlerWithoutConfigFilePath(cfg, manager)
	h.tokenStore = &memoryAuthStore{}
	return &concurrencyFixture{h: h, manager: manager, authDir: authDir}
}

func call(method, target, body string, handler func(*gin.Context)) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler(ctx)
	return rec
}

func (f *concurrencyFixture) patch(body string) *httptest.ResponseRecorder {
	return call(http.MethodPatch, "/v0/management/auth-files/fields", body, f.h.PatchAuthFileFields)
}

func (f *concurrencyFixture) batch(body string) *httptest.ResponseRecorder {
	return call(http.MethodPatch, "/v0/management/auth-files/fields/batch", body, f.h.PatchAuthFileFieldsBatch)
}

func (f *concurrencyFixture) entry(t *testing.T, name string) map[string]any {
	t.Helper()
	rec := call(http.MethodGet, "/v0/management/auth-files", "", f.h.ListAuthFiles)
	var body struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, file := range body.Files {
		if file["name"] == name {
			return file
		}
	}
	t.Fatalf("%s missing from %s", name, rec.Body.String())
	return nil
}

func TestPatchConcurrencyLimitInheritanceZeroAndInvalid(t *testing.T) {
	f := newManagementConcurrencyFixture(t, "a.json")
	if got := f.entry(t, "a.json"); got["concurrency_limit"] != float64(2) || got["concurrency_source"] != "default" || got["concurrency_override"] != nil || got["in_flight"] != float64(0) {
		t.Fatalf("default entry = %+v", got)
	}
	if rec := f.patch(`{"name":"a.json","concurrency_limit":8}`); rec.Code != 200 {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}
	if got := f.entry(t, "a.json"); got["concurrency_limit"] != float64(8) || got["concurrency_override"] != float64(8) || got["concurrency_source"] != "override" {
		t.Fatalf("override entry = %+v", got)
	}
	if rec := f.patch(`{"name":"a.json","concurrency_limit":0}`); rec.Code != 200 {
		t.Fatalf("zero: %d", rec.Code)
	}
	if got := f.entry(t, "a.json"); got["concurrency_limit"] != float64(0) || got["concurrency_override"] != float64(0) {
		t.Fatalf("zero entry = %+v", got)
	}
	if rec := f.patch(`{"name":"a.json","concurrency_limit":null}`); rec.Code != 200 {
		t.Fatalf("null: %d", rec.Code)
	}
	if got := f.entry(t, "a.json"); got["concurrency_source"] != "default" {
		t.Fatalf("restored entry = %+v", got)
	}
	for _, value := range []string{"-1", "1.5", `"3"`, "true", "[1]"} {
		if rec := f.patch(`{"name":"a.json","concurrency_limit":` + value + `}`); rec.Code != 400 {
			t.Fatalf("%s: %d", value, rec.Code)
		}
	}
	if rec := f.patch(`{"name":"missing.json","concurrency_limit":1}`); rec.Code != 404 {
		t.Fatalf("missing: %d", rec.Code)
	}
}

func TestPatchConcurrencyLimitLeavesAccountFileAlone(t *testing.T) {
	f := newManagementConcurrencyFixture(t, "a.json")
	path := filepath.Join(f.authDir, "a.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	record, _ := f.manager.GetByID("a.json")
	if rec := f.patch(`{"name":"a.json","concurrency_limit":3}`); rec.Code != 200 {
		t.Fatalf("set: %d", rec.Code)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("account file changed: %s %v", after, err)
	}
	updated, _ := f.manager.GetByID("a.json")
	if updated.Generation != record.Generation || updated.Metadata["concurrency_limit"] != nil {
		t.Fatalf("account changed: %+v", updated)
	}
}

func TestPatchConcurrencyLimitMixedFieldsRejectedBeforeAnyWrite(t *testing.T) {
	f := newManagementConcurrencyFixture(t, "a.json")
	for _, body := range []string{
		`{"name":"a.json","concurrency_limit":3,"note":"x"}`,
		`{"name":"a.json","concurrency_limit":3,"priority":1}`,
		`{"name":"a.json","concurrency_limit.nested":3}`,
	} {
		if rec := f.patch(body); rec.Code != 400 {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	record, _ := f.manager.GetByID("a.json")
	if record.Metadata["note"] != nil {
		t.Fatal("mixed patch wrote the account")
	}
	if got := f.entry(t, "a.json"); got["concurrency_source"] != "default" {
		t.Fatalf("mixed patch wrote the limit: %+v", got)
	}
	if rec := f.patch(`{"name":"a.json","note":"kept"}`); rec.Code != 200 {
		t.Fatalf("other fields must work as before: %d", rec.Code)
	}
}

func TestPatchConcurrencyLimitInapplicableAccount(t *testing.T) {
	f := newManagementConcurrencyFixture(t)
	record := &coreauth.Auth{ID: "c.json", FileName: "c.json", Provider: "claude", Attributes: map[string]string{"path": "/tmp/c.json"}}
	if _, err := f.manager.Register(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if rec := f.patch(`{"name":"c.json","concurrency_limit":1}`); rec.Code != 400 {
		t.Fatalf("code = %d", rec.Code)
	}
	if got := f.entry(t, "c.json"); got["concurrency_limit"] != nil {
		t.Fatalf("inapplicable entry = %+v", got)
	}
}

func TestPatchConcurrencyBatchPartialAndUnchanged(t *testing.T) {
	f := newManagementConcurrencyFixture(t, "a.json", "b.json")
	if rec := f.patch(`{"name":"b.json","concurrency_limit":8}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	rec := f.batch(`{"names":["a.json","a.json","b.json","missing.json"],"concurrency_limit":8}`)
	if rec.Code != 200 {
		t.Fatalf("batch: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Status    string   `json:"status"`
		Succeeded []string `json:"succeeded"`
		Unchanged []string `json:"unchanged"`
		Failed    []struct {
			Name string `json:"name"`
		} `json:"failed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "partial" || len(body.Succeeded) != 1 || body.Succeeded[0] != "a.json" || len(body.Unchanged) != 1 || body.Unchanged[0] != "b.json" || len(body.Failed) != 1 || body.Failed[0].Name != "missing.json" {
		t.Fatalf("batch result = %+v", body)
	}
	if rec = f.batch(`{"names":["a.json","b.json"],"concurrency_limit":null}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if got := f.entry(t, "a.json"); got["concurrency_source"] != "default" {
		t.Fatalf("restored = %+v", got)
	}
	for _, invalid := range []string{`{"names":[],"concurrency_limit":1}`, `{"names":["a.json"]}`, `{"concurrency_limit":1}`, `{"names":["a.json"],"concurrency_limit":-1}`, `{"names":["a.json"],"concurrency_limit":1,"note":"x"}`} {
		if rec = f.batch(invalid); rec.Code != 400 {
			t.Fatalf("%s: %d", invalid, rec.Code)
		}
	}
}

func TestConcurrencyWriteFailureKeepsOldValues(t *testing.T) {
	f := newManagementConcurrencyFixture(t, "a.json")
	// A regular file where the directory belongs fails every write deterministically.
	if err := os.WriteFile(filepath.Join(f.authDir, ".codex-account-concurrency"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := f.patch(`{"name":"a.json","concurrency_limit":3}`); rec.Code != 500 {
		t.Fatalf("single: %d", rec.Code)
	}
	if rec := f.batch(`{"names":["a.json"],"concurrency_limit":3}`); rec.Code != 500 {
		t.Fatalf("batch: %d", rec.Code)
	}
	if got := f.entry(t, "a.json"); got["concurrency_source"] != "default" {
		t.Fatalf("failed write changed memory: %+v", got)
	}
}

func TestDeleteAuthFileForgetsConcurrencyLimit(t *testing.T) {
	f := newManagementConcurrencyFixture(t, "a.json", "b.json")
	if rec := f.batch(`{"names":["a.json","b.json"],"concurrency_limit":5}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	rec := call(http.MethodDelete, "/v0/management/auth-files?name=a.json", "", f.h.DeleteAuthFile)
	if rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(f.authDir, ".codex-account-concurrency", "limits.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "a.json") || !strings.Contains(string(data), "b.json") {
		t.Fatalf("limits = %s", data)
	}
	if rec = call(http.MethodDelete, "/v0/management/auth-files?all=true", "", f.h.DeleteAuthFile); rec.Code != 200 {
		t.Fatalf("delete all: %d", rec.Code)
	}
	data, err = os.ReadFile(filepath.Join(f.authDir, ".codex-account-concurrency", "limits.json"))
	if err != nil || strings.Contains(string(data), "b.json") {
		t.Fatalf("limits after delete all = %s %v", data, err)
	}
}

func TestListAuthFilesReportsInFlightAndSwitch(t *testing.T) {
	f := newManagementConcurrencyFixture(t, "a.json")
	limit := 1
	cfg := &config.Config{AuthDir: f.authDir}
	cfg.Codex.AccountConcurrency = config.CodexAccountConcurrencyConfig{Enabled: true, DefaultLimit: &limit}
	f.manager.SetConfig(cfg)
	f.manager.RegisterExecutor(codexStubExecutor{})
	_, release, err := f.manager.AcquireAuthWithCredentialPolicy(context.Background(), "codex", "", coreauth.CredentialPolicyCodexAlphaSearchV1, coreexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if got := f.entry(t, "a.json"); got["in_flight"] != float64(1) || got["concurrency_limit"] != float64(1) {
		t.Fatalf("entry = %+v", got)
	}
	rec := call(http.MethodGet, "/v0/management/auth-files", "", f.h.ListAuthFiles)
	if !strings.Contains(rec.Body.String(), `"concurrency_enabled":true`) {
		t.Fatalf("list = %s", rec.Body.String())
	}
	rec = call(http.MethodGet, "/v0/management/auth-files?page=1&page_size=10", "", f.h.ListAuthFiles)
	if !strings.Contains(rec.Body.String(), `"in_flight":1`) || !strings.Contains(rec.Body.String(), `"concurrency_enabled":true`) {
		t.Fatalf("paged list = %s", rec.Body.String())
	}
}

type codexStubExecutor struct{}

func (codexStubExecutor) Identifier() string { return "codex" }
func (codexStubExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}
func (codexStubExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, nil
}
func (codexStubExecutor) Refresh(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	return a, nil
}
func (codexStubExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, nil
}
func (codexStubExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func concurrencyConfigHandler(t *testing.T, home bool) *Handler {
	t.Helper()
	cfg := &config.Config{}
	cfg.Home.Enabled = home
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	return &Handler{cfg: cfg, configFilePath: writeTestConfigFile(t), authManager: manager}
}

func TestCodexAccountConcurrencyConfigSaveAndValidation(t *testing.T) {
	h := concurrencyConfigHandler(t, false)
	put := func(body string) *httptest.ResponseRecorder {
		return call(http.MethodPut, "/v0/management/codex-account-concurrency/config", body, h.PutCodexAccountConcurrencyConfig)
	}
	rec := call(http.MethodGet, "/v0/management/codex-account-concurrency/config", "", h.GetCodexAccountConcurrencyConfig)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"default-limit":2`) || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	if rec = put(`{"enabled":true,"default-limit":0}`); rec.Code != 200 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	persisted, err := config.LoadConfig(h.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	got := persisted.Codex.AccountConcurrency
	if !got.Enabled || got.DefaultLimit == nil || *got.DefaultLimit != 0 {
		t.Fatalf("explicit zero lost: %+v", got)
	}
	for _, body := range []string{`{"default-limit":-1}`, `{"default-limit":1.5}`, `{"unknown":1}`, `{"enabled":true} {}`, `null`} {
		if rec = put(body); rec.Code != 400 {
			t.Fatalf("%s: %d", body, rec.Code)
		}
	}
	if h.cfg.Codex.AccountConcurrency.EffectiveDefaultLimit() != 0 {
		t.Fatal("invalid update mutated live config")
	}
	if rec = put(`{"default-limit":4}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if !h.cfg.Codex.AccountConcurrency.Enabled || h.cfg.Codex.AccountConcurrency.EffectiveDefaultLimit() != 4 {
		t.Fatalf("partial update: %+v", h.cfg.Codex.AccountConcurrency)
	}
}

func TestCodexAccountConcurrencyConfigRejectsEnableUnderHome(t *testing.T) {
	h := concurrencyConfigHandler(t, true)
	rec := call(http.MethodPut, "/v0/management/codex-account-concurrency/config", `{"enabled":true}`, h.PutCodexAccountConcurrencyConfig)
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d", rec.Code)
	}
	if h.cfg.Codex.AccountConcurrency.Enabled {
		t.Fatal("Home conflict mutated config")
	}
	if rec = call(http.MethodPut, "/v0/management/codex-account-concurrency/config", `{"enabled":false,"default-limit":3}`, h.PutCodexAccountConcurrencyConfig); rec.Code != 200 {
		t.Fatalf("disabled save under Home: %d", rec.Code)
	}
}
