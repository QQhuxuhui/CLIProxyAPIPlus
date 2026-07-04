package management

import (
	"testing"
	"time"
)

func TestParseUsageStatsRange(t *testing.T) {
	loc := time.Local
	from, to, err := parseUsageStatsRange("2026-06-27", "2026-07-03", loc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if from.Format("2006-01-02") != "2026-06-27" || to.Format("2006-01-02") != "2026-07-03" {
		t.Errorf("range = %s..%s", from.Format("2006-01-02"), to.Format("2006-01-02"))
	}
}

func TestParseUsageStatsRange_DefaultsToToday(t *testing.T) {
	loc := time.Local
	from, to, err := parseUsageStatsRange("", "", loc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	today := time.Now().In(loc).Format("2006-01-02")
	if from.Format("2006-01-02") != today || to.Format("2006-01-02") != today {
		t.Errorf("empty range should default to today %s, got %s..%s", today, from.Format("2006-01-02"), to.Format("2006-01-02"))
	}
}

func TestParseUsageStatsRange_ToBeforeFrom(t *testing.T) {
	if _, _, err := parseUsageStatsRange("2026-07-03", "2026-06-27", time.Local); err == nil {
		t.Error("to<from must error")
	}
}
