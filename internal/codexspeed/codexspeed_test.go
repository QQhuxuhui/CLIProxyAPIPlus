package codexspeed

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func histOf(ms ...int) Histogram {
	var h Histogram
	for _, v := range ms {
		h.Add(time.Duration(v) * time.Millisecond)
	}
	return h
}

func TestHistogramQuantileWithinBucketResolution(t *testing.T) {
	var h Histogram
	for i := 1; i <= 1000; i++ {
		h.Add(time.Duration(i*10) * time.Millisecond)
	}
	p50, ok := h.Quantile(0.5)
	if !ok || p50 < 4700 || p50 > 5300 {
		t.Fatalf("p50 = %d, want about 5000", p50)
	}
	if _, ok := Histogram(nil).Quantile(0.5); ok {
		t.Fatal("empty histogram must report no quantile")
	}
}

func TestHistogramMergeIgnoresTrafficSplit(t *testing.T) {
	// Same per-hour distribution, different traffic: merged p50 must agree.
	var control, treatment Histogram
	add := func(h *Histogram, n, ms int) {
		for i := 0; i < n; i++ {
			h.Add(time.Duration(ms) * time.Millisecond)
		}
	}
	hourly := [][2]int{{100000, 1000}, {3000, 9000}, {3000, 9000}}
	for _, hr := range hourly {
		var c, tr Histogram
		add(&c, hr[0], hr[1])
		add(&tr, hr[0]*15/85, hr[1])
		control.Merge(c)
		treatment.Merge(tr)
	}
	pc, _ := control.Quantile(0.5)
	pt, _ := treatment.Quantile(0.5)
	if pc != pt {
		t.Fatalf("control p50 %d != treatment p50 %d", pc, pt)
	}
}

func TestFastSessionLabel(t *testing.T) {
	many := func(ms int) GroupHour {
		var g GroupHour
		for i := 0; i < 60; i++ {
			g.Hist.Add(time.Duration(ms) * time.Millisecond)
		}
		return g
	}
	cases := []struct {
		name          string
		fast, control GroupHour
		want          string
	}{
		{"effective", many(1000), many(9000), LabelEffective},
		{"slower", many(9000), many(5000), LabelSlower},
		{"no difference", many(5000), many(5200), LabelNoDifference},
		{"few control", many(1000), GroupHour{Hist: histOf(9000, 9000)}, LabelInsufficient},
		{"empty fast", GroupHour{}, many(9000), LabelInsufficient},
	}
	for _, tc := range cases {
		if got := FastSessionLabel(tc.fast, tc.control); got != tc.want {
			t.Errorf("%s: got %s want %s", tc.name, got, tc.want)
		}
	}
}

func TestGroupHourFinalizeEmptyPercentilesNil(t *testing.T) {
	g := GroupHour{Requests: 3}
	g.Finalize()
	if g.P50Ms != nil || g.P90Ms != nil || g.Samples != 0 {
		t.Fatalf("empty group must have nil percentiles: %+v", g)
	}
}

func TestStoreAppendPrunesAndSkipsBadLines(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	old := HourRecord{Hour: now.Add(-8 * 24 * time.Hour)}
	recent := HourRecord{Hour: now.Add(-time.Hour), FastSession: &FastSessionHour{Members: 3}}
	if err := store.AppendHour(old, now.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, historyFile), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("{not json\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err = store.AppendHour(recent, now); err != nil {
		t.Fatal(err)
	}
	got := store.LoadHours(now)
	if len(got) != 1 || got[0].FastSession == nil || got[0].FastSession.Members != 3 {
		t.Fatalf("got %+v", got)
	}
	if filepath.Ext(historyFile) == ".json" {
		t.Fatal("history must not use the .json suffix")
	}
}

func TestRuntimeRecordKeepsMemoryOnDiskFailure(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	file := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt := NewRuntime(NewStore(filepath.Join(file, "sub")), func() time.Time { return now })
	rt.Record(HourRecord{Hour: now})
	if len(rt.Hours()) != 1 {
		t.Fatal("record must stay in memory when the disk write fails")
	}
}
