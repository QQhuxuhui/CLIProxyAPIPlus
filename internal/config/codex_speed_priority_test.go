package config

import (
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestCodexSpeedPriorityNormalizeDefaults(t *testing.T) {
	var c CodexSpeedPriorityConfig
	c.Normalize()
	want := CodexSpeedPriorityConfig{
		FastPercent: 30, ScoreWindowMinutes: 30, ScoreHalfLifeMinutes: 5, ScoreMinSamples: 1, ScoreOverloadPercent: 30,
		MaxReasoningTokens: 64, Probe: CodexSpeedProbeConfig{PercentPerMinute: 1.7, Model: "gpt-6-luna"},
	}
	if c != want {
		t.Fatalf("defaults: %+v", c)
	}
	c = CodexSpeedPriorityConfig{FastPercent: 500, ScoreWindowMinutes: 2, ScoreMinSamples: 3, Probe: CodexSpeedProbeConfig{PercentPerMinute: 50}}
	c.Normalize()
	if c.FastPercent != 90 || c.ScoreWindowMinutes != 5 || c.ScoreMinSamples != 3 || c.Probe.PercentPerMinute != 10 {
		t.Fatalf("clamp: %+v", c)
	}
	for in, want := range map[float64]float64{0.2: 1, 2.5: 2.5, -4: 1.7, math.NaN(): 1.7} {
		c = CodexSpeedPriorityConfig{Probe: CodexSpeedProbeConfig{PercentPerMinute: in}}
		c.Normalize()
		if c.Probe.PercentPerMinute != want {
			t.Fatalf("percent %v normalized to %v, want %v", in, c.Probe.PercentPerMinute, want)
		}
	}
	if got := (CodexSpeedProbeConfig{PercentPerMinute: 2}).Round(); got != 50*time.Minute {
		t.Fatalf("round = %v, want 50m", got)
	}
	if got := (CodexSpeedProbeConfig{}).Round(); got != 0 {
		t.Fatalf("round without a percent = %v, want 0", got)
	}
}

// Configs written before the percent setting carry an interval and a per-second cap; both are
// ignored on load and dropped on save, while an explicit sample floor is kept.
func TestCodexSpeedPriorityDropsIntervalAndRateKeys(t *testing.T) {
	path := writeConfig(t, `port: 8317
codex:
  speed-priority:
    enable: true
    score-min-samples: 3
    probe:
      model: gpt-6-luna
      interval-minutes: 10
      rate-per-second: 3
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Codex.SpeedPriority
	if got.Probe.PercentPerMinute != 1.7 || got.ScoreMinSamples != 3 {
		t.Fatalf("loaded: %+v", got)
	}
	if err = SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := speedPriorityBlock(string(data))
	for _, stale := range []string{"interval-minutes", "rate-per-second"} {
		if strings.Contains(text, stale) {
			t.Fatalf("stale key %q survived save:\n%s", stale, text)
		}
	}
	for _, kept := range []string{"percent-per-minute: 1.7", "score-min-samples: 3"} {
		if !strings.Contains(text, kept) {
			t.Fatalf("key %q missing after save:\n%s", kept, text)
		}
	}
}

// v98 persisted every normalized field; v99 must ignore those keys and drop them on save.
const codexSpeedV98Config = `port: 8317
codex:
  speed-priority:
    enable: true
    percent: 50
    weights:
      fast: 10
      unknown: 3
      slow: 1
    fast-gap-ms: 3000
    fast-max-overload-percent: 15
    slow-overload-percent: 30
    min-samples: 3
    sample-window-minutes: 60
    half-life-minutes: 30
    baseline-min-ms: 2500
    max-reasoning-tokens: 64
    probe:
      per-account-interval-minutes: 60
      max-per-second: 1
      model: gpt-6-luna
    rollback:
      window-minutes: 30
      min-samples: 40
`

func TestCodexSpeedPriorityIgnoresV98Keys(t *testing.T) {
	path := writeConfig(t, codexSpeedV98Config)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Codex.SpeedPriority
	if !got.Enable || got.FastPercent != 30 || got.ScoreWindowMinutes != 30 || got.ScoreHalfLifeMinutes != 5 ||
		got.Probe.PercentPerMinute != 1.7 || got.Probe.Model != "gpt-6-luna" {
		t.Fatalf("v98 keys must not apply: %+v", got)
	}
	if err = SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := speedPriorityBlock(string(data))
	for _, stale := range []string{"percent", "weights", "fast-gap-ms", "fast-max-overload-percent", "slow-overload-percent", "min-samples", "sample-window-minutes", "half-life-minutes", "baseline-min-ms", "per-account-interval-minutes", "max-per-second", "rollback"} {
		if regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(stale) + `:`).MatchString(text) {
			t.Fatalf("stale key %q survived save:\n%s", stale, text)
		}
	}
	for _, kept := range []string{"enable: true", "fast-percent: 30", "percent-per-minute: 1.7", "model: gpt-6-luna"} {
		if !strings.Contains(text, kept) {
			t.Fatalf("key %q missing after save:\n%s", kept, text)
		}
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Codex.SpeedPriority != got {
		t.Fatalf("round trip changed values: %+v vs %+v", reloaded.Codex.SpeedPriority, got)
	}
}

// speedPriorityBlock returns the lines of the codex.speed-priority mapping, dedented.
func speedPriorityBlock(text string) string {
	lines := strings.Split(text, "\n")
	var out []string
	indent := -1
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		depth := len(line) - len(trimmed)
		if indent < 0 {
			if trimmed == "speed-priority:" {
				indent = depth
			}
			continue
		}
		if trimmed != "" && depth <= indent {
			break
		}
		out = append(out, strings.TrimPrefix(line, strings.Repeat(" ", indent+2)))
	}
	return strings.Join(out, "\n")
}
