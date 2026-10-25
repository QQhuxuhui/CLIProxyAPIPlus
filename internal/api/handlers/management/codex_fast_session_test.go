package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

func TestGetCodexFastSessionReportsConfigAndGroups(t *testing.T) {
	cfg := &config.Config{}
	cfg.Codex.FastSessionIDs = config.CodexFastSessionIDsConfig{Enable: true, Percent: 25, Seed: []string{"fast-a", "fast-b"}}
	h := &Handler{cfg: cfg}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/codex-fast-session", nil)
	h.GetCodexFastSession(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got helps.CodexFastSessionSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Enable || got.Percent != 25 || !slices.Equal(got.Members, []string{"fast-a", "fast-b"}) {
		t.Fatalf("snapshot = %+v", got)
	}
	if _, ok := got.Groups[helps.CodexFastSessionGroupFast]; !ok {
		t.Fatalf("missing fast group: %+v", got.Groups)
	}
	if _, ok := got.Groups[helps.CodexFastSessionGroupControl]; !ok {
		t.Fatalf("missing control group: %+v", got.Groups)
	}
	if got.Events == nil || got.Rollback.Disabled || got.Groups[helps.CodexFastSessionGroupFast].WindowMinutes != 10 {
		t.Fatalf("status fields = %+v", got)
	}
}
