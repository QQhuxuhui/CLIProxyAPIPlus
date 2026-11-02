package helps

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexspeed"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestCodexFastSessionHoursRollAndLabel(t *testing.T) {
	pool := NewCodexFastSessionPool()
	pool.ApplyConfig(config.CodexFastSessionIDsConfig{Enable: true, Percent: 90, Seed: []string{"a", "b"}})
	now := time.Date(2026, 11, 2, 6, 10, 0, 0, time.UTC)
	pool.nowFunc = func() time.Time { return now }
	for i := 0; i < 60; i++ {
		pool.Observe(CodexFastSessionGroupFast, false, time.Second, 100)
		pool.Observe(CodexFastSessionGroupControl, false, 9*time.Second, 100)
	}
	pool.Observe(CodexFastSessionGroupFast, true, 0, 100)
	pool.Observe(CodexFastSessionGroupOff, false, time.Second, 100)

	cur := pool.CurrentHour(now)
	if cur.Fast.Requests != 61 || cur.Fast.Failures != 1 || cur.Control.Samples != 60 || cur.Label != codexspeed.LabelEffective {
		t.Fatalf("current hour = %+v", cur)
	}
	if got := pool.RollHours(now); len(got) != 0 {
		t.Fatalf("unfinished hour must not roll: %+v", got)
	}
	now = now.Add(time.Hour)
	pool.Observe(CodexFastSessionGroupFast, false, time.Second, 100)
	rolled := pool.RollHours(now)
	if len(rolled) != 1 || !rolled[0].Hour.Equal(time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("rolled = %+v", rolled)
	}
	if rolled[0].Members != 2 || !rolled[0].Enabled || rolled[0].Label != codexspeed.LabelEffective || rolled[0].Control.P50Ms == nil {
		t.Fatalf("rolled record = %+v", rolled[0])
	}
	if again := pool.RollHours(now); len(again) != 0 {
		t.Fatalf("rolled hours must be forgotten: %+v", again)
	}
	if cur := pool.CurrentHour(now); cur.Fast.Requests != 1 {
		t.Fatalf("new hour = %+v", cur)
	}
}

func TestCodexFastSessionHoursRestartOnEpochChange(t *testing.T) {
	pool := NewCodexFastSessionPool()
	pool.ApplyConfig(config.CodexFastSessionIDsConfig{Enable: true, Percent: 90, Seed: []string{"a"}})
	now := time.Date(2026, 11, 2, 6, 10, 0, 0, time.UTC)
	pool.nowFunc = func() time.Time { return now }
	pool.Observe(CodexFastSessionGroupControl, false, time.Second, 100)
	pool.ApplyConfig(config.CodexFastSessionIDsConfig{Enable: true, Percent: 50, Seed: []string{"a"}})
	pool.Observe(CodexFastSessionGroupControl, false, time.Second, 100)
	cur := pool.CurrentHour(now)
	if cur.Control.Requests != 1 || cur.EffectivePercent != 50 {
		t.Fatalf("hour must restart under the new epoch: %+v", cur)
	}
}

func TestCodexFastSessionHoursEmptyPoolInsufficient(t *testing.T) {
	pool := NewCodexFastSessionPool()
	now := time.Date(2026, 11, 2, 6, 10, 0, 0, time.UTC)
	cur := pool.CurrentHour(now)
	if cur.Label != codexspeed.LabelInsufficient || cur.Members != 0 || cur.Fast.P50Ms != nil {
		t.Fatalf("empty pool hour = %+v", cur)
	}
}
