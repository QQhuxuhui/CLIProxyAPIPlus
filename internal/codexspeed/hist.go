// Package codexspeed keeps the hourly history behind the Codex speed strategy page: fast
// Session-Id group stats, speed-priority A/B stats, and their on-disk persistence.
package codexspeed

import (
	"math"
	"time"
)

// HistBins is the number of log-spaced TTFT buckets between histMinMs and histMaxMs.
const HistBins = 128

const (
	histMinMs = 100.0
	histMaxMs = 300000.0
)

var histRatio = math.Pow(histMaxMs/histMinMs, 1.0/HistBins)

// Histogram counts TTFT samples in HistBins log-spaced buckets. Bucket counts add up exactly
// across hours, so merged percentiles do not depend on how traffic was split between hours.
// A nil histogram is empty.
type Histogram []uint32

func histBin(ms float64) int {
	if ms <= histMinMs {
		return 0
	}
	if ms >= histMaxMs {
		return HistBins - 1
	}
	bin := int(math.Log(ms/histMinMs) / math.Log(histRatio))
	return min(max(bin, 0), HistBins-1)
}

// Add records one sample.
func (h *Histogram) Add(d time.Duration) {
	if h == nil || d <= 0 {
		return
	}
	if len(*h) != HistBins {
		*h = make(Histogram, HistBins)
	}
	(*h)[histBin(float64(d)/float64(time.Millisecond))]++
}

// Merge adds src into h.
func (h *Histogram) Merge(src Histogram) {
	if h == nil || len(src) != HistBins {
		return
	}
	if len(*h) != HistBins {
		*h = make(Histogram, HistBins)
	}
	for i, v := range src {
		(*h)[i] += v
	}
}

// Count returns the number of samples.
func (h Histogram) Count() uint64 {
	var n uint64
	for _, v := range h {
		n += uint64(v)
	}
	return n
}

// Quantile returns the q-th quantile (0<q<=1) in milliseconds, interpolated log-linearly
// inside the bucket that holds the nearest-rank sample. ok is false for an empty histogram.
func (h Histogram) Quantile(q float64) (int64, bool) {
	n := h.Count()
	if n == 0 || len(h) != HistBins {
		return 0, false
	}
	rank := uint64(math.Ceil(q * float64(n)))
	rank = min(max(rank, 1), n)
	var cum uint64
	for i, v := range h {
		if v == 0 {
			continue
		}
		if cum+uint64(v) >= rank {
			frac := (float64(rank-cum) - 0.5) / float64(v)
			lower := histMinMs * math.Pow(histRatio, float64(i))
			return int64(math.Round(lower * math.Pow(histRatio, frac))), true
		}
		cum += uint64(v)
	}
	return 0, false
}
