package cliproxy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagestats"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// TestServiceShutdown_FlushesUsageStats verifies the wiring added to fix the
// "usagestats.Flush() is never called on shutdown" finding: Service.Shutdown
// must persist any usage-stats counts accumulated since the last periodic
// flush, so a graceful shutdown never silently drops in-memory counters
// (usagestats' own periodic flush loop only runs every 30s).
func TestServiceShutdown_FlushesUsageStats(t *testing.T) {
	dir := t.TempDir()
	usagestats.Init(dir)
	usagestats.SetEnabled(true)
	t.Cleanup(func() { usagestats.SetEnabled(false) })

	const account = "shutdown-flush-test-account"
	const model = "shutdown-flush-test-model"
	now := time.Now()

	usage.PublishRecord(context.Background(), usage.Record{
		AuthIndex:   account,
		Model:       model,
		RequestedAt: now,
	})

	// usage.PublishRecord enqueues on an async background worker; poll until
	// the record has actually reached the aggregator before asserting state.
	deadline := time.Now().Add(2 * time.Second)
	for {
		stats, err := usagestats.Query(now, now, account)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(stats) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage record was not aggregated within timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}

	dayFile := filepath.Join(dir, now.Format("2006-01-02")+".json")
	if _, err := os.Stat(dayFile); err == nil {
		t.Fatalf("day file %s should not exist yet (periodic flush is every 30s)", dayFile)
	}

	svc := &Service{}
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if _, err := os.Stat(dayFile); err != nil {
		t.Fatalf("expected usage-stats day file %s to be persisted by Shutdown, stat error: %v", dayFile, err)
	}
}
