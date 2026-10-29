package helps

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type egressTestClock struct{ t time.Time }

func (c *egressTestClock) now() time.Time          { return c.t }
func (c *egressTestClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newEgressTestPool(t *testing.T, cfg config.CodexEgressPoolConfig, random *float64) (*CodexEgressPool, *egressTestClock) {
	t.Helper()
	clock := &egressTestClock{t: time.Date(2026, 10, 29, 12, 0, 30, 0, time.UTC)}
	p := newCodexEgressPool(clock.now, func() float64 { return *random })
	p.ApplyConfig(cfg)
	return p, clock
}

func egressTestConfig(proxies ...string) config.CodexEgressPoolConfig {
	cfg := config.CodexEgressPoolConfig{Enable: true}
	for _, u := range proxies {
		cfg.Proxies = append(cfg.Proxies, config.CodexEgressProxyConfig{URL: u, MaxConcurrency: 2})
	}
	return cfg
}

func egressRecord(failed bool, status int, ttft time.Duration) usage.Record {
	r := usage.Record{Failed: failed, TTFT: ttft, Generate: usage.GenerateFlag(true)}
	if failed {
		r.Fail = usage.Failure{StatusCode: status}
	}
	return r
}

func TestCodexEgressMaskAndUnmask(t *testing.T) {
	raw := "http://user1:p+ss@isp.example.com:10003"
	masked := MaskCodexEgressProxyURL(raw)
	if masked != "http://user1:***@isp.example.com:10003" {
		t.Fatalf("masked = %q", masked)
	}
	current := config.CodexEgressPoolConfig{Proxies: []config.CodexEgressProxyConfig{{URL: raw}}}
	next := config.CodexEgressPoolConfig{Proxies: []config.CodexEgressProxyConfig{{URL: masked}, {URL: "socks5://h:1"}}}
	got := UnmaskCodexEgressConfig(next, current)
	if got.Proxies[0].URL != raw || got.Proxies[1].URL != "socks5://h:1" {
		t.Fatalf("unmask = %+v", got.Proxies)
	}
	if next.Proxies[0].URL != masked {
		t.Fatal("unmask mutated its input")
	}
	snap := MaskCodexEgressConfig(current)
	if strings.Contains(snap.Proxies[0].URL, "p+ss") || current.Proxies[0].URL != raw {
		t.Fatalf("mask config = %+v / %+v", snap.Proxies, current.Proxies)
	}
}

func TestCodexEgressRetryRoutingControlStickinessAndCapacity(t *testing.T) {
	random := 0.5
	p, _ := newEgressTestPool(t, egressTestConfig("http://a:1", "http://b:1"), &random)

	first := p.selectRoute("auth-1", false)
	if first.group != CodexEgressGroupFirstDirect || first.proxy != nil {
		t.Fatalf("first attempt without shift = %+v", first)
	}

	random = 0.05 // below the default 10% control share
	if d := p.selectRoute("auth-1", true); d.group != CodexEgressGroupRetryControl || d.proxy != nil {
		t.Fatalf("control retry = %+v", d)
	}

	random = 0.5
	d1 := p.selectRoute("auth-1", true)
	if d1.group != CodexEgressGroupRetryPool || d1.proxy == nil {
		t.Fatalf("pool retry = %+v", d1)
	}
	p.release(d1.proxy)
	d2 := p.selectRoute("auth-1", true)
	if d2.proxy != d1.proxy {
		t.Fatal("same credential should keep its preferred proxy")
	}
	// Fill every slot (2 per proxy): the preferred proxy first, then a spill to the other one.
	d3 := p.selectRoute("auth-1", true)
	d4 := p.selectRoute("auth-1", true)
	d5 := p.selectRoute("auth-1", true)
	if d3.proxy != d2.proxy || d4.proxy == nil || d4.proxy == d2.proxy || d5.proxy != d4.proxy {
		t.Fatalf("expected spill to the second proxy: %+v %+v %+v", d3, d4, d5)
	}
	if d6 := p.selectRoute("auth-1", true); d6.proxy != nil || d6.group != CodexEgressGroupRetryFallback {
		t.Fatalf("full pool should fall back: %+v", d6)
	}
}

func TestCodexEgressConnectFailureCooldownDoubles(t *testing.T) {
	random := 0.5
	p, clock := newEgressTestPool(t, egressTestConfig("http://a:1"), &random)
	d := p.selectRoute("auth", true)
	d = p.reselect(d, "auth", errors.New("proxy CONNECT returned status 429 Too Many Requests"))
	if d.proxy != nil {
		t.Fatal("only proxy failed; expected direct")
	}
	px := p.proxies[0]
	if px.inflight != 0 || px.cooldownUntil.Sub(clock.now()) != 30*time.Second {
		t.Fatalf("inflight=%d cooldown=%s", px.inflight, px.cooldownUntil.Sub(clock.now()))
	}
	if got := p.selectRoute("auth", true); got.group != CodexEgressGroupRetryFallback {
		t.Fatalf("cooling proxy must be skipped: %+v", got)
	}
	clock.advance(31 * time.Second)
	d = p.selectRoute("auth", true)
	p.reselect(d, "auth", errors.New("dial HTTP proxy failed"))
	if got := px.cooldownUntil.Sub(clock.now()); got != 60*time.Second {
		t.Fatalf("second cooldown = %s, want 60s", got)
	}
	snap := p.Snapshot()
	if snap.Proxies[0].Status != "cooling" || snap.Routes[1].Points[59].ConnFail != 1 {
		t.Fatalf("snapshot proxy=%+v points=%+v", snap.Proxies[0], snap.Routes[1].Points[59])
	}
}

func TestCodexEgressShiftFollowsDirectOverload(t *testing.T) {
	random := 0.5
	cfg := egressTestConfig("http://a:1")
	cfg.Shift.Enable = true
	p, clock := newEgressTestPool(t, cfg, &random)
	feed := func(n, overloads int) {
		for i := 0; i < n; i++ {
			p.observe(CodexEgressGroupFirstDirect, CodexEgressRouteDirect, egressRecord(i < overloads, 502, time.Second))
		}
	}
	feed(60, 30)
	clock.advance(time.Minute)
	p.selectRoute("x", false)
	if p.shiftPercent != 20 {
		t.Fatalf("shift = %d, want 20", p.shiftPercent)
	}
	feed(60, 30)
	clock.advance(time.Minute)
	p.selectRoute("x", false)
	if p.shiftPercent != 30 {
		t.Fatalf("shift = %d, want 30", p.shiftPercent)
	}
	random = 0.1
	if d := p.selectRoute("x", false); d.group != CodexEgressGroupShiftPool || d.proxy == nil {
		t.Fatalf("shifted first attempt = %+v", d)
	}
	// Overload falls away: the 5-minute window drains and the share steps back down.
	clock.advance(6 * time.Minute)
	feed(100, 5)
	p.selectRoute("x", false)
	if p.shiftPercent != 20 {
		t.Fatalf("shift = %d, want 20 after recovery", p.shiftPercent)
	}
}

func TestCodexEgressRollbackAndTrial(t *testing.T) {
	random := 0.5
	cfg := egressTestConfig("http://a:1")
	cfg.Rollback.Enable = true
	cfg.Shift.Enable = true
	p, clock := newEgressTestPool(t, cfg, &random)
	for i := 0; i < 50; i++ {
		p.observe(CodexEgressGroupRetryPool, "http://a:1", egressRecord(i < 10, 502, time.Second))
		p.observe(CodexEgressGroupRetryControl, CodexEgressRouteDirect, egressRecord(i < 12, 502, time.Second))
	}
	clock.advance(time.Minute)
	p.selectRoute("x", true)
	if p.disabledUntil.IsZero() || !strings.Contains(p.rollbackMessage, "retry_pool failure") {
		t.Fatalf("expected rollback for a 4-point gain: until=%v msg=%q", p.disabledUntil, p.rollbackMessage)
	}
	if d := p.selectRoute("x", true); d.proxy != nil || d.group != CodexEgressGroupRetryFallback {
		t.Fatalf("rolled back pool must route direct: %+v", d)
	}
	clock.advance(11 * time.Minute)
	d := p.selectRoute("x", true)
	if !p.trial || d.proxy == nil {
		t.Fatalf("trial should route retries to the pool: trial=%v d=%+v", p.trial, d)
	}
	p.release(d.proxy)
	p.shiftPercent = 50
	random = 0.01
	if d := p.selectRoute("x", false); d.proxy != nil {
		t.Fatal("trial must not shift first attempts")
	}
	random = 0.5
	for i := 0; i < 50; i++ {
		p.observe(CodexEgressGroupRetryPool, "http://a:1", egressRecord(i < 2, 502, time.Second))
		p.observe(CodexEgressGroupRetryControl, CodexEgressRouteDirect, egressRecord(i < 15, 502, time.Second))
	}
	clock.advance(time.Minute)
	p.selectRoute("x", true)
	if p.trial || p.rollbackMessage != "ok" {
		t.Fatalf("trial should pass: trial=%v msg=%q", p.trial, p.rollbackMessage)
	}
}

type egressFakeRT struct {
	err   error
	calls int
}

func (f *egressFakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if req.Body != nil {
		_, _ = io.ReadAll(req.Body)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Request: req}, nil
}

func TestCodexEgressRoundTripperFallsBackOnProxyConnectFailure(t *testing.T) {
	random := 0.5
	p, _ := newEgressTestPool(t, egressTestConfig("http://a:1"), &random)
	proxyRT := &egressFakeRT{err: &codexEgressConnectError{err: errors.New("proxy CONNECT returned status 429 Too Many Requests")}}
	p.proxies[0].transport = proxyRT
	direct := &egressFakeRT{}
	reporter := &UsageReporter{}
	rt := &codexEgressRoundTripper{pool: p, direct: direct, authKey: "auth", retry: true, reporter: reporter}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", strings.NewReader(`{"a":1}`))
	resp, err := rt.RoundTrip(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if proxyRT.calls != 1 || direct.calls != 1 {
		t.Fatalf("proxy calls=%d direct calls=%d", proxyRT.calls, direct.calls)
	}
	if reporter.codexEgressGroup != CodexEgressGroupRetryFallback || reporter.codexEgressRoute != CodexEgressRouteDirect {
		t.Fatalf("reporter tagged %q/%q", reporter.codexEgressGroup, reporter.codexEgressRoute)
	}
	if p.proxies[0].inflight != 0 || p.proxies[0].failStreak != 1 {
		t.Fatalf("proxy state inflight=%d streak=%d", p.proxies[0].inflight, p.proxies[0].failStreak)
	}
}

func TestCodexEgressRoundTripperReleasesSlotOnBodyClose(t *testing.T) {
	random := 0.5
	p, _ := newEgressTestPool(t, egressTestConfig("http://a:1"), &random)
	p.proxies[0].transport = &egressFakeRT{}
	reporter := &UsageReporter{}
	rt := &codexEgressRoundTripper{pool: p, direct: &egressFakeRT{}, authKey: "auth", retry: true, reporter: reporter}
	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", strings.NewReader(`{}`))
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if p.proxies[0].inflight != 1 || reporter.codexEgressGroup != CodexEgressGroupRetryPool || reporter.codexEgressRoute != "http://a:1" {
		t.Fatalf("inflight=%d group=%q route=%q", p.proxies[0].inflight, reporter.codexEgressGroup, reporter.codexEgressRoute)
	}
	_ = resp.Body.Close()
	_ = resp.Body.Close()
	if p.proxies[0].inflight != 0 {
		t.Fatalf("inflight after close = %d", p.proxies[0].inflight)
	}
	p.observe(reporter.codexEgressGroup, reporter.codexEgressRoute, egressRecord(true, 503, 0))
	if s := p.Snapshot(); s.Groups[CodexEgressGroupRetryPool].Attempts != 1 || s.Groups[CodexEgressGroupRetryPool].OverloadRate != 1 {
		t.Fatalf("group stats = %+v", s.Groups[CodexEgressGroupRetryPool])
	}
}

func TestCodexEgressConfigReloadKeepsProxyState(t *testing.T) {
	random := 0.5
	cfg := egressTestConfig("http://a:1")
	p, _ := newEgressTestPool(t, cfg, &random)
	d := p.selectRoute("auth", true)
	if d.proxy == nil {
		t.Fatal("expected proxy")
	}
	cfg.Proxies[0].MaxConcurrency = 5
	p.ApplyConfig(cfg)
	if p.proxies[0] != d.proxy || p.proxies[0].inflight != 1 || p.proxies[0].maxConc != 5 {
		t.Fatalf("reload lost state: %+v", p.proxies[0])
	}
	cfg.Enable = false
	p.ApplyConfig(cfg)
	if p.Active() {
		t.Fatal("disabled pool must be inactive")
	}
}
