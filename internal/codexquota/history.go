package codexquota

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

const (
	historyRetention = 30 * 24 * time.Hour
	sampleInterval   = 5 * time.Minute
	minBurnCoverage  = 15 * time.Minute
)

// sample is one persisted pool-level history entry; it holds no per-account data.
type sample struct {
	Time         time.Time          `json:"t"`
	Usable       int                `json:"usable"`
	Capacity     int                `json:"capacity"`
	PeakRPM      int                `json:"peak_rpm"`
	AvgRemaining float64            `json:"avg_remaining"`
	Burn         map[string]float64 `json:"burn,omitempty"`
}

type history struct {
	samples []sample
}

func burnKey(plan string, minutes int) string {
	return plan + "|" + strconv.Itoa(minutes)
}

func (h *history) add(item sample, now time.Time) {
	h.samples = append(h.samples, item)
	h.trim(now)
}

func (h *history) trim(now time.Time) {
	cutoff := now.Add(-historyRetention)
	index := 0
	for index < len(h.samples) && h.samples[index].Time.Before(cutoff) {
		index++
	}
	h.samples = h.samples[index:]
}

func (h *history) covered(now time.Time) time.Duration {
	if len(h.samples) == 0 {
		return 0
	}
	return now.Sub(h.samples[0].Time) + sampleInterval
}

// burnRate returns percent-units consumed per hour over the span, or nil when
// too little history exists to measure it.
func (h *history) burnRate(plan string, minutes int, span time.Duration, now time.Time) *float64 {
	covered := min(span, h.covered(now))
	if covered < minBurnCoverage {
		return nil
	}
	cutoff := now.Add(-span)
	key := burnKey(plan, minutes)
	total := 0.0
	for _, item := range h.samples {
		// Each sample covers the interval before its timestamp, so the cutoff is exclusive.
		if item.Time.After(cutoff) {
			total += item.Burn[key]
		}
	}
	rate := round1(total / covered.Hours())
	return &rate
}

func (h *history) peakRPM(span time.Duration, now time.Time) int {
	cutoff := now.Add(-span)
	peak := 0
	for _, item := range h.samples {
		if !item.Time.Before(cutoff) {
			peak = max(peak, item.PeakRPM)
		}
	}
	return peak
}

func (h *history) trend(span time.Duration, now time.Time) []TrendPoint {
	cutoff := now.Add(-span)
	points := []TrendPoint{}
	for _, item := range h.samples {
		if !item.Time.Before(cutoff) {
			points = append(points, TrendPoint{Time: item.Time, Usable: item.Usable, Capacity: item.Capacity, PeakRPM: item.PeakRPM, AvgRemaining: item.AvgRemaining})
		}
	}
	return points
}

func loadHistory(path string, now time.Time) (*history, error) {
	out := &history{}
	file, errOpen := os.Open(path)
	if errors.Is(errOpen, os.ErrNotExist) {
		return out, nil
	}
	if errOpen != nil {
		return out, fmt.Errorf("open history: %w", errOpen)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var item sample
		// A partial last line after a crash is expected and safe to skip.
		if errParse := json.Unmarshal(scanner.Bytes(), &item); errParse == nil && !item.Time.IsZero() {
			out.samples = append(out.samples, item)
		}
	}
	out.trim(now)
	if errScan := scanner.Err(); errScan != nil {
		return out, fmt.Errorf("read history: %w", errScan)
	}
	return out, nil
}

func appendHistory(path string, item sample) error {
	data, errMarshal := json.Marshal(item)
	if errMarshal != nil {
		return fmt.Errorf("marshal history: %w", errMarshal)
	}
	file, errOpen := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if errOpen != nil {
		return fmt.Errorf("open history: %w", errOpen)
	}
	_, errWrite := file.Write(append(data, '\n'))
	errClose := file.Close()
	if errWrite != nil {
		return fmt.Errorf("write history: %w", errWrite)
	}
	if errClose != nil {
		return fmt.Errorf("close history: %w", errClose)
	}
	return nil
}

func rewriteHistory(path string, samples []sample) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	for _, item := range samples {
		if errEncode := encoder.Encode(item); errEncode != nil {
			return fmt.Errorf("encode history: %w", errEncode)
		}
	}
	return writeFileAtomic(path, buffer.Bytes())
}
