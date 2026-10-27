package registry

import (
	"sync"
	"testing"
	"time"
)

func TestPoolRegistryReadsIgnoreWriterLock(t *testing.T) {
	r := &ModelRegistry{models: make(map[string]*ModelRegistration), clientModels: make(map[string][]string), clientModelInfos: make(map[string]map[string]*ModelInfo), clientProviders: make(map[string]string), mutex: &sync.RWMutex{}}
	r.RegisterClient("a", "codex", []*ModelInfo{{ID: "model", OwnedBy: "test"}})
	r.EnablePoolReads()
	r.mutex.Lock()
	defer r.mutex.Unlock()
	done := make(chan bool, 1)
	go func() {
		models, epoch := r.GetModelsAndEpochForClient("a")
		_, err := r.GetFirstAvailableModel("")
		done <- r.ClientSupportsModel("a", "model") && len(models) == 1 && epoch > 0 && len(r.GetModelProviders("model")) == 1 && r.GetModelInfo("model", "") != nil && err == nil
	}()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("snapshot lost model data")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot reader waited for registry writer")
	}
}
func TestPoolRegistryClientEpochAndDeletion(t *testing.T) {
	r := &ModelRegistry{models: make(map[string]*ModelRegistration), clientModels: make(map[string][]string), clientModelInfos: make(map[string]map[string]*ModelInfo), clientProviders: make(map[string]string), mutex: &sync.RWMutex{}}
	r.EnablePoolReads()
	r.RegisterClient("a", "codex", []*ModelInfo{{ID: "one"}})
	first := r.ClientRegistrationEpoch("a")
	r.RegisterClient("a", "codex", []*ModelInfo{{ID: "two"}})
	if r.ClientRegistrationEpoch("a") <= first || r.ClientSupportsModel("a", "one") || !r.ClientSupportsModel("a", "two") {
		t.Fatal("registration snapshot was stale")
	}
	r.UnregisterClient("a")
	if r.ClientSupportsModel("a", "two") || r.GetModelInfo("two", "") != nil {
		t.Fatal("deleted model survived snapshot")
	}
}

func TestPoolRegistryQuotaAndNormalizedProjectionPublish(t *testing.T) {
	r := newTestModelRegistry()
	r.EnablePoolReads()
	r.RegisterClient("a", "codex", []*ModelInfo{{ID: "model"}})
	r.SetModelQuotaExceededUntil("a", "model", time.Now().Add(time.Minute))
	if r.GetModelCount("model") != 0 {
		t.Fatal("quota setter left stale availability")
	}
	r.ClearModelQuotaExceeded("a", "model")
	if !r.ApplyClientModelProjections("a", r.ClientRegistrationEpoch("a"), 1, []ClientModelProjection{{ModelID: " model ", Suspended: true, SuspendReason: "transient"}}) {
		t.Fatal("projection rejected")
	}
	if r.GetModelCount("model") != 0 {
		t.Fatal("normalized projection left stale availability")
	}
}
func TestPoolRegistryBatchPublicationAdvancesVisibleGeneration(t *testing.T) {
	r := newTestModelRegistry()
	r.EnablePoolReads()
	finish := r.BeginPoolRegistrationBatch()
	r.RegisterClient("a", "codex", []*ModelInfo{{ID: "model"}})
	before := r.GetGeneration()
	if r.GetModelInfo("model", "") != nil {
		t.Fatal("batch directory published early")
	}
	finish()
	if r.GetGeneration() == before {
		t.Fatal("directory changed without a visible generation change")
	}
}

func TestPoolRegistryAvailabilityRemainsLiveDuringDirectoryBatch(t *testing.T) {
	r := newTestModelRegistry()
	r.EnablePoolReads()
	r.RegisterClient("a", "codex", []*ModelInfo{{ID: "model"}})
	finish := r.BeginPoolRegistrationBatch()
	defer finish()
	r.ApplyClientModelProjections("a", r.ClientRegistrationEpoch("a"), 1, []ClientModelProjection{{ModelID: "model", Suspended: true, SuspendReason: "transient"}})
	if r.GetModelCount("model") != 0 {
		t.Fatal("directory batch froze live availability")
	}
}

func TestPoolRegistryTracksFreshManagerIncarnation(t *testing.T) {
	r := newTestModelRegistry()
	r.EnablePoolReads()
	r.RegisterClient("a", "codex", []*ModelInfo{{ID: "model"}})
	r.TrackClientAuthEpoch("a", 100)
	r.UnregisterClient("a")
	r.TrackClientAuthEpoch("a", 1)
	r.RegisterClient("a", "codex", []*ModelInfo{{ID: "model"}})
	ok := r.ApplyClientModelProjectionBatch([]ClientProjectionUpdate{{ClientID: "a", AuthEpoch: 1, Epoch: r.ClientRegistrationEpoch("a"), Generation: 1, Projections: []ClientModelProjection{{ModelID: "model"}}}})
	if !ok[0] {
		t.Fatal("fresh manager's credential was rejected by a previous manager epoch")
	}
}
