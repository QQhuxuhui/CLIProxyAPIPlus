package pluginhost

import (
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// hostReentrantExecutorManager mimics the auth manager's lock ordering: while it is inside
// an executor mutation it calls back into the host the same way the request path does under
// the manager lock (scheduler lookup -> Host.mu).
type hostReentrantExecutorManager struct {
	*fakeExecutorManager
	host *Host
}

func (m *hostReentrantExecutorManager) RegisterExecutor(executor coreauth.ProviderExecutor) {
	m.host.SchedulerWantsAcrossPriorities()
	m.fakeExecutorManager.RegisterExecutor(executor)
}

func (m *hostReentrantExecutorManager) UnregisterExecutor(provider string) {
	m.host.SchedulerWantsAcrossPriorities()
	m.fakeExecutorManager.UnregisterExecutor(provider)
}

func TestRegisterExecutorsDoesNotHoldHostLockWhileCallingManager(t *testing.T) {
	host := newHostWithRecords(capabilityRecord{
		id: "alpha",
		plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
			Executor: &fakeExecutor{identifier: "provider"},
		}},
	})
	manager := &hostReentrantExecutorManager{fakeExecutorManager: newFakeExecutorManager(), host: host}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Register path.
		host.RegisterExecutors(manager, nil)
		// Unregister path: the provider becomes stale once the plugin leaves the snapshot.
		setHostSnapshotForTest(host, true)
		host.RegisterExecutors(manager, nil)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RegisterExecutors deadlocked: Host.mu is held while calling into the executor manager")
	}

	if manager.registerCalls != 1 {
		t.Fatalf("RegisterExecutor calls = %d, want 1", manager.registerCalls)
	}
	if len(manager.unregisters) != 1 || manager.unregisters[0] != "provider" {
		t.Fatalf("unregisters = %#v, want [provider]", manager.unregisters)
	}
}
