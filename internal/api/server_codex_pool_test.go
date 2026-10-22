package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCodexPoolEmbeddedChartAsset(t *testing.T) {
	s := newTestServer(t)
	s.cfg.Home.Enabled = false
	s.cfg.RemoteManagement.DisableControlPanel = false
	rec := httptest.NewRecorder()
	s.engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/quota-monitor/echarts.min.js", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "javascript") || !strings.Contains(rec.Body.String(), "Apache") {
		t.Fatalf("asset response: %d %v", rec.Code, rec.Header())
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}
	for _, home := range []bool{false, true} {
		s.cfg.Home.Enabled = home
		s.cfg.RemoteManagement.DisableControlPanel = !home
		rec = httptest.NewRecorder()
		s.engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/quota-monitor/echarts.min.js", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("disabled panel asset: %d", rec.Code)
		}
	}
}
