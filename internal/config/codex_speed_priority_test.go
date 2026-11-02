package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestCodexSpeedPriorityNormalizeDefaults(t *testing.T) {
	var c CodexSpeedPriorityConfig
	c.Normalize()
	want := CodexSpeedPriorityConfig{
		FastPercent: 30, ScoreWindowMinutes: 30, ScoreHalfLifeMinutes: 5, ScoreMinSamples: 3, ScoreOverloadPercent: 30,
		MaxReasoningTokens: 64, Probe: CodexSpeedProbeConfig{IntervalMinutes: 10, RatePerSecond: 3, Model: "gpt-6-luna"},
	}
	if c != want {
		t.Fatalf("defaults: %+v", c)
	}
	c = CodexSpeedPriorityConfig{FastPercent: 500, ScoreWindowMinutes: 2, Probe: CodexSpeedProbeConfig{IntervalMinutes: 500, RatePerSecond: 20}}
	c.Normalize()
	if c.FastPercent != 90 || c.ScoreWindowMinutes != 5 || c.Probe.IntervalMinutes != 60 || c.Probe.RatePerSecond != 3 {
		t.Fatalf("clamp: %+v", c)
	}
	c = CodexSpeedPriorityConfig{Probe: CodexSpeedProbeConfig{RatePerSecond: 1}}
	c.Normalize()
	if c.Probe.RatePerSecond != 1 {
		t.Fatalf("lower rate must be kept: %+v", c.Probe)
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
		got.Probe.IntervalMinutes != 10 || got.Probe.RatePerSecond != 3 || got.Probe.Model != "gpt-6-luna" {
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
	for _, kept := range []string{"enable: true", "fast-percent: 30", "interval-minutes: 10", "model: gpt-6-luna"} {
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
