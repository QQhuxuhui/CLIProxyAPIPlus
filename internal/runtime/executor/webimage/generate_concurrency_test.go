package webimage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// A busy account must not block: acquire returns a retryable capacity signal
// immediately so the conductor can rotate to an idle account instead of queuing
// on the occupied one, while other accounts stay acquirable.
func TestAcquireBusyAccountReturnsBusyWithoutBlocking(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	cfg.WebImageMaxConcurrency = 1
	cfg.WebImageMaxConcurrencyPerAccount = 1
	executor := NewExecutor(cfg)

	occupiedAccountSlot := make(chan struct{}, 1)
	occupiedAccountSlot <- struct{}{}
	executor.accountMu.Lock()
	executor.accountSlots["account-a"] = occupiedAccountSlot
	executor.accountMu.Unlock()

	done := make(chan error, 1)
	go func() {
		release, errAcquire := executor.acquire(context.Background(), "account-a")
		if errAcquire == nil {
			release()
		}
		done <- errAcquire
	}()

	var errBusy error
	select {
	case errBusy = <-done:
	case <-time.After(time.Second):
		t.Fatal("acquire on a busy account blocked; want immediate busy signal")
	}
	if errBusy == nil {
		t.Fatal("acquire on a busy account returned no error; want busy capacity signal")
	}
	var statusErr *StatusError
	if !errors.As(errBusy, &statusErr) {
		t.Fatalf("busy error type = %T, want *StatusError", errBusy)
	}
	if !statusErr.AccountBusy() {
		t.Fatal("StatusError.AccountBusy() = false, want true for a per-account limit hit")
	}
	if statusErr.StatusCode() != 429 {
		t.Fatalf("busy StatusError code = %d, want 429", statusErr.StatusCode())
	}

	// A different account is unaffected and still acquires successfully.
	releaseOther, errOther := executor.acquire(context.Background(), "account-b")
	if errOther != nil {
		t.Fatalf("account-b acquire error = %v, want success while account-a is busy", errOther)
	}
	releaseOther()
}

func TestAcquireRemovesIdleAccountSlot(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetWebImageDefaults()
	executor := NewExecutor(cfg)

	release, errAcquire := executor.acquire(context.Background(), "temporary-account")
	if errAcquire != nil {
		t.Fatalf("acquire() error = %v", errAcquire)
	}
	release()

	executor.accountMu.Lock()
	accountCount := len(executor.accountSlots)
	executor.accountMu.Unlock()
	if accountCount != 0 {
		t.Fatalf("account slot count = %d, want 0 after release", accountCount)
	}
}
