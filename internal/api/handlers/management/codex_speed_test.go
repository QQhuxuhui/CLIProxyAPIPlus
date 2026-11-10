package management

import (
	"encoding/json"
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
	if rec := call(`{"speed_priority":{"fast_percent":25,"probe_interval_minutes":20}}`); rec.Code != 200 {
		t.Fatalf("fast percent save: %d %s", rec.Code, rec.Body.String())
	}
	got := h.cfg.Codex.SpeedPriority
	if got.Enable || got.FastPercent != 25 || got.Probe.IntervalMinutes != 20 || got.Probe.RatePerSecond != 3 {
		t.Fatalf("saved: %+v", got)
	}
	for _, body := range []string{`{"speed_priority":{"fast_percent":0}}`, `{"speed_priority":{"fast_percent":91}}`, `{"speed_priority":{"probe_interval_minutes":61}}`, `{"speed_priority":{"percent":20}}`, `{"speed_priority":{"weights":{"fast":2}}}`, `{"speed_priority":{"x":1}}`} {
		if rec := call(body); rec.Code != 400 {
			t.Fatalf("invalid %s: %d", body, rec.Code)
		}
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

func TestCodexSpeedPriorityViewShape(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	h.cfg.Codex.SpeedPriority.FastPercent = 40
	legacy := codexspeed.SpeedHour{Hour: time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC), Schema: codexspeed.SpeedSchemaLegacyAB}
	legacy.Overall.Hist.Add(time.Second)
	view := h.codexSpeedPriorityView(h.cfg, []codexspeed.HourRecord{{Hour: legacy.Hour, Speed: &legacy}, {Hour: legacy.Hour.Add(time.Hour)}})
	if view.Config.FastPercent != 40 || view.Config.ProbeIntervalMinutes != 10 {
		t.Fatalf("config: %+v", view.Config)
	}
	if len(view.Hours) != 1 || view.Hours[0].Schema != codexspeed.SpeedSchemaLegacyAB || view.Hours[0].Overall.Hist != nil {
		t.Fatalf("hours: %+v", view.Hours)
	}
	if len(view.Live.Bins) != 50 {
		t.Fatalf("bins: %d", len(view.Live.Bins))
	}
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"fast_percent"`, `"probe_interval_minutes"`, `"bins"`, `"overflow_share"`, `"ready_min"`, `"schema"`} {
		if !strings.Contains(string(data), key) {
			t.Fatalf("missing %s in %s", key, data)
		}
	}
}
