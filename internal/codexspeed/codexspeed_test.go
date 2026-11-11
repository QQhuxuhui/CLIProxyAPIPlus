package codexspeed

import (
	"os"
	"path/filepath"
	"strings"
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

func TestSpeedGroupHourMerge(t *testing.T) {
	a := SpeedGroupHour{Attempts: 2, Hist: histOf(1000)}
	a.Merge(SpeedGroupHour{Attempts: 3, Overloads: 1, Hist: histOf(1000, 1000)})
	if a.Attempts != 5 || a.Overloads != 1 || a.Samples != 3 || a.P50Ms == nil {
		t.Fatalf("merge: %+v", a)
	}
}

func TestSpeedHourOverflowShare(t *testing.T) {
	h := SpeedHour{Fast: SpeedGroupHour{Attempts: 3}, Slow: SpeedGroupHour{Attempts: 1}}
	h.FinalizeGroups()
	if h.OverflowShare == nil || *h.OverflowShare != 0.25 {
		t.Fatalf("share = %v", h.OverflowShare)
	}
	empty := SpeedHour{}
	empty.FinalizeGroups()
	if empty.OverflowShare != nil {
		t.Fatal("no attempts must leave the share nil")
	}
}

// A v98 A/B line with a second generation segment: treatment and control merge into overall,
// the schema marks it legacy, and a rewrite plus reload keeps the result without double counting.
const legacyHourLine = `{"hour":"2026-11-02T11:00:00Z","fast_session":{"hour":"2026-11-02T11:00:00Z","members":2,"label":"no_difference"},` +
	`"speed":{"hour":"2026-11-02T11:00:00Z","generation":"g1@x","treatment":{"attempts":3,"overloads":1,"ttft_hist":[0,0]},"control":{"attempts":2,"overloads":0}},` +
	`"speed_parts":[{"hour":"2026-11-02T11:00:00Z","generation":"g2@y","treatment":{"attempts":4},"control":{"attempts":1,"overloads":1}}]}`

func TestLegacyHourMigratesOnceAndKeepsFastSession(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, historyFile), []byte(legacyHourLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(dir)
	now := time.Date(2026, 11, 2, 13, 0, 0, 0, time.UTC)
	check := func(stage string, recs []HourRecord) {
		t.Helper()
		var legacy *HourRecord
		for i := range recs {
			if recs[i].Hour.Equal(time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC)) {
				legacy = &recs[i]
			}
		}
		if legacy == nil || legacy.Speed == nil || legacy.FastSession == nil || legacy.FastSession.Members != 2 {
			t.Fatalf("%s: legacy record lost: %+v", stage, recs)
		}
		sp := legacy.Speed
		if sp.Schema != SpeedSchemaLegacyAB || sp.Overall.Attempts != 10 || sp.Overall.Overloads != 2 || sp.Fast.Attempts != 0 {
			t.Fatalf("%s: migrated speed = %+v", stage, sp)
		}
	}
	check("load", store.LoadHours(now))
	if err := store.AppendHour(HourRecord{Hour: now, Speed: &SpeedHour{Hour: now, Schema: SpeedSchemaRanked}}, now); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, historyFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "speed_parts") || strings.Contains(string(data), "treatment") {
		t.Fatalf("legacy fields written back:\n%s", data)
	}
	recs := NewStore(dir).LoadHours(now)
	check("reload", recs)
	if len(recs) != 2 || recs[1].Speed.Schema != SpeedSchemaRanked {
		t.Fatalf("ranked record must keep its schema: %+v", recs)
	}
}

func TestStoreSamplesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if got := store.LoadSamples(); len(got.Accounts) != 0 || got.ProbeCycle != 0 {
		t.Fatalf("missing file must load empty, got %+v", got)
	}
	want := SampleSnapshot{ProbeCycle: 129 * time.Minute, Accounts: []AccountSamples{
		{ID: "a.json", LastProbe: 1700000000000, Samples: [][3]int64{{1700000000000, 2500, 1}, {1700000100000, 900, 0}}},
		{ID: "b.json", Samples: [][3]int64{{1700000200000, 4000, 1}}},
	}}
	if err := store.SaveSamples(want); err != nil {
		t.Fatalf("SaveSamples: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, samplesFile), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open samples: %v", err)
	}
	if _, errWrite := f.WriteString("{broken\n"); errWrite != nil {
		t.Fatalf("append: %v", errWrite)
	}
	if errClose := f.Close(); errClose != nil {
		t.Fatalf("close: %v", errClose)
	}
	got := store.LoadSamples()
	if got.ProbeCycle != want.ProbeCycle || len(got.Accounts) != 2 {
		t.Fatalf("loaded cycle %v, %d accounts", got.ProbeCycle, len(got.Accounts))
	}
	if got.Accounts[0].ID != "a.json" || got.Accounts[0].LastProbe != 1700000000000 || got.Accounts[0].Samples[1] != [3]int64{1700000100000, 900, 0} {
		t.Fatalf("account 0 = %+v", got.Accounts[0])
	}
	if strings.HasSuffix(samplesFile, ".json") {
		t.Fatal("samples file must not look like a credential file")
	}
}
