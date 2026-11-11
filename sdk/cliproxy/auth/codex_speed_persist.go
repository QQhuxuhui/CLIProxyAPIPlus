package auth

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/codexspeed"
)

// ExportSamples snapshots every account's samples that can still count toward a score.
func (t *CodexSpeedTracker) ExportSamples() codexspeed.SampleSnapshot {
	var snap codexspeed.SampleSnapshot
	if t == nil {
		return snap
	}
	cfg := t.config()
	now := t.now()
	snap.ProbeCycle = t.view.Load().probeCycle
	passive := time.Duration(cfg.ScoreWindowMinutes) * time.Minute
	probeValid := probeWindow(cfg, snap.ProbeCycle)
	for i := range t.shards {
		sh := &t.shards[i]
		sh.mu.Lock()
		for id, a := range sh.accounts {
			acc := codexspeed.AccountSamples{ID: id}
			// Walk the ring oldest first so an import replays samples in arrival order.
			for k := 0; k < a.count; k++ {
				s := a.samples[(a.next-a.count+k+codexSpeedRingSize)%codexSpeedRingSize]
				window, probe := passive, int64(0)
				if s.probe {
					window, probe = probeValid, 1
				}
				if age := now.Sub(s.at); age < 0 || age > window {
					continue
				}
				acc.Samples = append(acc.Samples, [3]int64{s.at.UnixMilli(), s.gap.Milliseconds(), probe})
			}
			if len(acc.Samples) == 0 {
				continue
			}
			if !a.lastProbe.IsZero() {
				acc.LastProbe = a.lastProbe.UnixMilli()
			}
			snap.Accounts = append(snap.Accounts, acc)
		}
		sh.mu.Unlock()
	}
	return snap
}

// ImportSamples restores a snapshot into a freshly started tracker and returns the number of
// accounts restored. Samples keep their original timestamps, so the usual validity windows
// still expire them; accounts that already hold samples are left alone.
func (t *CodexSpeedTracker) ImportSamples(snap codexspeed.SampleSnapshot) int {
	if t == nil {
		return 0
	}
	now := t.now()
	restored := 0
	for _, acc := range snap.Accounts {
		if acc.ID == "" {
			continue
		}
		sh := t.shard(acc.ID)
		sh.mu.Lock()
		if existing := sh.accounts[acc.ID]; existing != nil && existing.count > 0 {
			sh.mu.Unlock()
			continue
		}
		var samples []codexSpeedSample
		for _, s := range acc.Samples {
			at := time.UnixMilli(s[0])
			if s[1] <= 0 || at.After(now) || now.Sub(at) > codexSpeedEntryTTL {
				continue
			}
			samples = append(samples, codexSpeedSample{at: at, gap: time.Duration(s[1]) * time.Millisecond, probe: s[2] != 0})
		}
		if len(samples) == 0 {
			sh.mu.Unlock()
			continue
		}
		a := t.account(sh, acc.ID, now)
		for _, s := range samples[max(0, len(samples)-codexSpeedRingSize):] {
			a.addSampleLocked(s)
		}
		if lp := time.UnixMilli(acc.LastProbe); acc.LastProbe > 0 && !lp.After(now) && a.lastProbe.IsZero() {
			a.lastProbe = lp
		}
		sh.mu.Unlock()
		restored++
	}
	// The first recompute sizes the probe validity window from the previous view's cycle, so
	// seed it; otherwise restored probe samples would look expired for one round.
	if prev := t.view.Load(); restored > 0 && prev.probeCycle == 0 && snap.ProbeCycle > 0 && snap.ProbeCycle <= codexSpeedEntryTTL {
		seeded := emptyCodexSpeedView()
		seeded.probeCycle = snap.ProbeCycle
		t.view.CompareAndSwap(prev, seeded)
	}
	return restored
}
