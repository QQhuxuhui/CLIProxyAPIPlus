package webimage

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestAcquireWaitingForAccountDoesNotReserveGlobalSlot(t *testing.T) {
	previousMaxProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previousMaxProcs)

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

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterStarted := make(chan struct{})
	waiterDone := make(chan error, 1)
	go func() {
		close(waiterStarted)
		release, errAcquire := executor.acquire(waiterCtx, "account-a")
		if errAcquire == nil {
			release()
		}
		waiterDone <- errAcquire
	}()
	<-waiterStarted
	runtime.Gosched()
	defer func() {
		cancelWaiter()
		select {
		case <-waiterDone:
		case <-time.After(time.Second):
			t.Error("account-a waiter did not stop")
		}
	}()

	otherCtx, cancelOther := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancelOther()
	releaseOther, errOther := executor.acquire(otherCtx, "account-b")
	if errOther != nil {
		t.Fatalf("account-b acquire error = %v, want success while account-a waits", errOther)
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
