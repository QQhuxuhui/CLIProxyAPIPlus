package auth

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func resetCodexRetryBudgetLimits(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { setCodexRetryBudgetLimits(0, 0) })
}

func TestCodexRetryBudgetLimitsDefaults(t *testing.T) {
	resetCodexRetryBudgetLimits(t)
	setCodexRetryBudgetLimits(0, 0)
	accounts, window := currentCodexRetryBudgetLimits()
	if accounts != 6 || window != 20*time.Second {
		t.Fatalf("defaults = %d, %s", accounts, window)
	}
	setCodexRetryBudgetLimits(-1, -1)
	if accounts, window = currentCodexRetryBudgetLimits(); accounts != 6 || window != 20*time.Second {
		t.Fatalf("negative values = %d, %s", accounts, window)
	}
}

func TestCodexRetryBudgetLimitsAreCapped(t *testing.T) {
	resetCodexRetryBudgetLimits(t)
	setCodexRetryBudgetLimits(1000, 100000)
	accounts, window := currentCodexRetryBudgetLimits()
	if accounts != 30 || window != 120*time.Second {
		t.Fatalf("capped = %d, %s", accounts, window)
	}
}

func TestCodexRetryBudgetSnapshotsLimitsPerRequest(t *testing.T) {
	resetCodexRetryBudgetLimits(t)
	setCodexRetryBudgetLimits(2, 60)
	before := time.Now()
	ctx := WithCodexRetryBudget(context.Background())
	setCodexRetryBudgetLimits(9, 5)
	b := codexBudget(ctx)
	if b == nil {
		t.Fatal("no budget")
	}
	if !b.claim("a") || !b.claim("b") {
		t.Fatal("claims within the limit must pass")
	}
	if b.claim("c") {
		t.Fatal("request must keep the limit it started with")
	}
	if b.deadline.Sub(before) < 59*time.Second {
		t.Fatalf("deadline uses the snapshot window: %s", b.deadline.Sub(before))
	}
}

func TestCodexRetryBudgetZeroValueUsesDefaultAccounts(t *testing.T) {
	b := &codexRetryBudget{now: time.Now, deadline: time.Now().Add(time.Minute), attempted: map[string]struct{}{}}
	if b.accountLimit() != defaultCodexRetryAccounts {
		t.Fatalf("limit = %d", b.accountLimit())
	}
}

func TestSetConfigPublishesCodexRetryBudgetLimits(t *testing.T) {
	resetCodexRetryBudgetLimits(t)
	m := NewManager(nil, nil, nil)
	cfg := &internalconfig.Config{}
	cfg.Codex.RetryBudget.MaxAccounts = 12
	cfg.Codex.RetryBudget.WindowSeconds = 45
	m.SetConfig(cfg)
	accounts, window := currentCodexRetryBudgetLimits()
	if accounts != 12 || window != 45*time.Second {
		t.Fatalf("published = %d, %s", accounts, window)
	}
	m.SetConfig(&internalconfig.Config{})
	if accounts, window = currentCodexRetryBudgetLimits(); accounts != 6 || window != 20*time.Second {
		t.Fatalf("removing the keys restores defaults: %d, %s", accounts, window)
	}
}

func TestCodexRetryBudgetLimitsStayConsistentDuringUpdates(t *testing.T) {
	resetCodexRetryBudgetLimits(t)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				setCodexRetryBudgetLimits(1, 120)
			} else {
				setCodexRetryBudgetLimits(30, 1)
			}
		}
	}()
	for i := 0; i < 200000; i++ {
		accounts, window := currentCodexRetryBudgetLimits()
		if (accounts == 30 && window == 120*time.Second) || (accounts == 1 && window == time.Second) {
			close(stop)
			<-done
			t.Fatalf("mixed limits: %d, %s", accounts, window)
		}
	}
	close(stop)
	<-done
}
