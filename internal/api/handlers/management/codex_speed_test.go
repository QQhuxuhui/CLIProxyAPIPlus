package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexquota"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexspeed"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexSpeedConfigSaveAndValidation(t *testing.T) {
	h := &Handler{cfg: &config.Config{}, configFilePath: writeTestConfigFile(t)}
	call := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/codex-speed/config", strings.NewReader(body))
		h.PutCodexSpeedConfig(ctx)
		return rec
	}
	if rec := call(`{"fast_session":{"enable":true,"percent":40}}`); rec.Code != 200 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	got := h.cfg.Codex.FastSessionIDs
	if !got.Enable || got.Percent != 40 {
		t.Fatalf("saved: %+v", got)
	}
	persisted, err := config.LoadConfig(h.configFilePath)
	if err != nil || !persisted.Codex.FastSessionIDs.Enable || persisted.Codex.FastSessionIDs.Percent != 40 {
		t.Fatalf("persisted: %+v %v", persisted.Codex.FastSessionIDs, err)
	}
	if rec := call(`{"fast_session":{"enable":false}}`); rec.Code != 200 || h.cfg.Codex.FastSessionIDs.Enable || h.cfg.Codex.FastSessionIDs.Percent != 40 {
		t.Fatalf("partial update: %d %+v", rec.Code, h.cfg.Codex.FastSessionIDs)
	}
	for _, body := range []string{`{"fast_session":{"percent":101}}`, `{"fast_session":{"percent":-1}}`, `{"other":1}`, `{} {}`} {
		if rec := call(body); rec.Code != 400 {
			t.Fatalf("invalid %s: %d", body, rec.Code)
		}
	}
}

func TestCodexSpeedSnapshotEndpoint(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	h.GetCodexSpeed(ctx)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	fs, ok := view["fast_session"].(map[string]any)
	if !ok || fs["hours"] == nil || fs["labels_24h"] == nil || fs["current"] == nil {
		t.Fatalf("view: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "ttft_hist") {
		t.Fatal("raw histograms must not be exposed")
	}
}

func TestCodexSpeedPriorityConfigRequiresRunningPools(t *testing.T) {
	h := &Handler{cfg: &config.Config{}, configFilePath: writeTestConfigFile(t), authManager: coreauth.NewManager(nil, nil, nil)}
	call := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/codex-speed/config", strings.NewReader(body))
		h.PutCodexSpeedConfig(ctx)
		return rec
	}
	if rec := call(`{"speed_priority":{"enable":true}}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "pools") {
		t.Fatalf("pools off: %d %s", rec.Code, rec.Body.String())
	}
	h.cfg.Codex.Pools.Enable = true
	if rec := call(`{"speed_priority":{"enable":true}}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "restart") {
		t.Fatalf("pools not running: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(`{"speed_priority":{"percent":20,"weights":{"fast":12,"slow":2}}}`); rec.Code != 200 {
		t.Fatalf("weights save: %d %s", rec.Code, rec.Body.String())
	}
	got := h.cfg.Codex.SpeedPriority
	if got.Enable || got.Percent != 20 || got.Weights.Fast != 12 || got.Weights.Slow != 2 || got.Weights.Unknown != 3 {
		t.Fatalf("saved: %+v", got)
	}
	for _, body := range []string{`{"speed_priority":{"percent":0}}`, `{"speed_priority":{"weights":{"fast":1001}}}`, `{"speed_priority":{"x":1}}`} {
		if rec := call(body); rec.Code != 400 {
			t.Fatalf("invalid %s: %d", body, rec.Code)
		}
	}
}

func TestCodexSpeedFastWearJudgedPerDay(t *testing.T) {
	day := func(d string, bSet, bConv, hSet, hConv int) codexspeed.SpeedHour {
		return codexspeed.SpeedHour{Generation: "g", Hour: time.Date(2026, 11, 3, 1, 0, 0, 0, time.UTC),
			Boost: codexspeed.SpeedCohortDay{Day: d, FastSet: bSet, FastToSlow: bConv},
			Hold:  codexspeed.SpeedCohortDay{Day: d, FastSet: hSet, FastToSlow: hConv}}
	}
	var small []codexspeed.SpeedHour
	for i := 1; i <= 7; i++ {
		small = append(small, day(fmt.Sprintf("2026-11-0%d", i), 3, 0, 3, 0))
	}
	if got := codexSpeedAcceptanceFor("g", small, codexspeed.SpeedHour{}).Checks[2]; got.Status != "insufficient" {
		t.Fatalf("7 days of 3 accounts must stay insufficient: %+v", got)
	}
	mixed := []codexspeed.SpeedHour{day("2026-11-01", 30, 3, 30, 3), day("2026-11-02", 30, 9, 30, 3), day("2026-11-03", 5, 5, 5, 0)}
	acc := codexSpeedAcceptanceFor("g", mixed, codexspeed.SpeedHour{})
	if acc.Checks[2].Status != "fail" || acc.Days[1].WearStatus != "fail" || acc.Days[2].WearStatus != "insufficient" {
		t.Fatalf("one failing day must fail: %+v %+v", acc.Checks[2], acc.Days)
	}
	if got := codexSpeedAcceptanceFor("g", mixed[:1], codexspeed.SpeedHour{}).Checks[2]; got.Status != "pass" {
		t.Fatalf("all judged days passing: %+v", got)
	}
}

func TestCodexSpeedQuotaReadingsKeepDuplicates(t *testing.T) {
	rows := map[string]codexquota.AccountRow{
		"a.json": {AccountKey: "acct", Duplicate: true, Windows: []codexquota.RowWindow{{Minutes: 43200, UsedPercent: 12}}},
		"b.json": {AccountKey: "acct", Duplicate: true, Windows: []codexquota.RowWindow{{Minutes: 43200, UsedPercent: 12}}},
		"c.json": {AccountKey: "other", Invalid: true, Windows: []codexquota.RowWindow{{Minutes: 43200, UsedPercent: 5}}},
		"d.json": {AccountKey: "pend", Windows: []codexquota.RowWindow{{Minutes: 43200, Pending: true}}},
	}
	got := codexSpeedQuotaReadings(rows)
	if len(got) != 2 || got["a.json"].AccountKey != "acct" || got["b.json"].UsedPercent != 12 {
		t.Fatalf("readings = %+v", got)
	}
}
