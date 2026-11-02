package config

import "testing"

func TestCodexSpeedPriorityNormalizeDefaults(t *testing.T) {
	var c CodexSpeedPriorityConfig
	c.Normalize()
	if c.Enable || c.Percent != 15 || c.Weights != (CodexSpeedWeights{Fast: 10, Unknown: 3, Slow: 1}) {
		t.Fatalf("defaults: %+v", c)
	}
	if c.FastGapMs != 3000 || c.FastMaxOverloadPercent != 15 || c.SlowOverloadPercent != 30 || c.MinSamples != 3 ||
		c.SampleWindowMinutes != 60 || c.HalfLifeMinutes != 30 || c.BaselineMinMs != 2500 || c.MaxReasoningTokens != 64 {
		t.Fatalf("thresholds: %+v", c)
	}
	if c.Probe.PerAccountIntervalMinutes != 60 || c.Probe.MaxPerSecond != 1 || c.Probe.Model != "gpt-6-luna" {
		t.Fatalf("probe: %+v", c.Probe)
	}
	if !c.RollbackEnabled() || c.Rollback.WindowMinutes != 30 || c.Rollback.MinSamples != 40 || c.Rollback.FailureWorsePoints != 10 ||
		c.Rollback.MinPercent != 2 || c.Rollback.RecoverIntervalSeconds != 600 || c.Rollback.RecoverStepPercent != 5 {
		t.Fatalf("rollback: %+v", c.Rollback)
	}
	c = CodexSpeedPriorityConfig{Percent: 500, Weights: CodexSpeedWeights{Fast: 5000}}
	c.Normalize()
	if c.Percent != 100 || c.Weights.Fast != 1000 {
		t.Fatalf("clamp: %+v", c)
	}
}
