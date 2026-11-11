package codexspeed

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
)

const samplesFile = "samples.jsonl"

// AccountSamples is one account's persisted speed samples. Each sample is
// [unix milliseconds, gap milliseconds, 1 for a probe sample or 0].
type AccountSamples struct {
	ID        string     `json:"id"`
	LastProbe int64      `json:"lp,omitempty"`
	Samples   [][3]int64 `json:"s"`
}

// SampleSnapshot is the persisted state that lets ranking resume after a restart.
type SampleSnapshot struct {
	ProbeCycle time.Duration
	Accounts   []AccountSamples
}

type samplesHeader struct {
	Version      int   `json:"v"`
	ProbeCycleMs int64 `json:"probe_cycle_ms"`
}

// SaveSamples replaces the samples file: a header line, then one line per account.
func (s *Store) SaveSamples(snap SampleSnapshot) error {
	if s == nil || s.dir == "" {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(samplesHeader{Version: 1, ProbeCycleMs: snap.ProbeCycle.Milliseconds()}); err != nil {
		return fmt.Errorf("codex speed: encode samples: %w", err)
	}
	for i := range snap.Accounts {
		if err := enc.Encode(&snap.Accounts[i]); err != nil {
			return fmt.Errorf("codex speed: encode samples: %w", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeFileAtomic(s.dir, samplesFile, buf.Bytes())
}

// LoadSamples reads the samples file, skipping malformed lines. A missing or unreadable file
// yields an empty snapshot.
func (s *Store) LoadSamples() SampleSnapshot {
	var snap SampleSnapshot
	if s == nil || s.dir == "" {
		return snap
	}
	s.mu.Lock()
	data, err := os.ReadFile(filepath.Join(s.dir, samplesFile))
	s.mu.Unlock()
	if err != nil {
		if !os.IsNotExist(err) {
			log.WithError(err).Warn("codex speed: read samples failed")
		}
		return snap
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	if !scanner.Scan() {
		return snap
	}
	var header samplesHeader
	if errHeader := json.Unmarshal(scanner.Bytes(), &header); errHeader != nil || header.Version != 1 {
		log.Warn("codex speed: samples file has an unknown header, ignored")
		return snap
	}
	snap.ProbeCycle = time.Duration(header.ProbeCycleMs) * time.Millisecond
	bad := 0
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var acc AccountSamples
		if errLine := json.Unmarshal(line, &acc); errLine != nil || acc.ID == "" {
			bad++
			continue
		}
		snap.Accounts = append(snap.Accounts, acc)
	}
	if bad > 0 {
		log.Warnf("codex speed: skipped %d malformed sample lines", bad)
	}
	return snap
}
