package registry

import (
	"fmt"
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

func TestPoolRegistryUnchangedRegistrationIsNoop(t *testing.T) {
	r := newTestModelRegistry()
	r.EnablePoolReads()
	models := []*ModelInfo{{ID: "one", Name: "fixture"}}
	r.RegisterClient("a", "codex", models)
	before, epoch, generation := r.poolDirectory.Load(), r.ClientRegistrationEpoch("a"), r.GetGeneration()
	r.RegisterClient("a", "codex", models)
	if before != r.poolDirectory.Load() || epoch != r.ClientRegistrationEpoch("a") || generation != r.GetGeneration() {
		t.Fatal("unchanged registration rebuilt snapshots")
	}
}
func TestPoolRegistryIncrementalKeepsUnchangedDefinition(t *testing.T) {
	r := newTestModelRegistry()
	r.EnablePoolReads()
	r.RegisterClient("a", "codex", []*ModelInfo{{ID: "one"}})
	before := r.poolDirectory.Load()
	r.RegisterClient("b", "codex", []*ModelInfo{{ID: "two"}})
	after := r.poolDirectory.Load()
	if before.view.models["one"] != after.view.models["one"] {
		t.Fatal("unrelated model definition rebuilt")
	}
	if len(before.view.models) != 1 || before.view.models["two"] != nil {
		t.Fatal("published snapshot mutated")
	}
}

func BenchmarkPoolRegistryRegistration(b *testing.B) {
	for _, n := range []int{1000, 2000, 4000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			for range b.N {
				r := newTestModelRegistry()
				r.EnablePoolReads()
				for i := range n {
					r.RegisterClient(fmt.Sprint(i), "codex", []*ModelInfo{{ID: "one"}, {ID: "two"}})
				}
			}
		})
	}
}
func TestPoolRegistryIncrementalCapabilities(t *testing.T) {
	r := newTestModelRegistry()
	r.EnablePoolReads()
	yes, no := true, false
	r.RegisterClient("a", "codex", []*ModelInfo{{ID: "one", SupportsWebSearch: true, NativeCapabilities: &NativeCapabilities{WebSearch: &yes}}})
	r.RegisterClient("b", "codex", []*ModelInfo{{ID: "one", NativeCapabilities: &NativeCapabilities{WebSearch: &no}}})
	if v := r.GetResponsesWebSearchCapability("one"); v == nil || *v {
		t.Fatal("explicit false route ignored")
	}
	if !r.GetModelInfo("one", "").SupportsWebSearch {
		t.Fatal("positive capability aggregate lost")
	}
	r.UnregisterClient("a")
	if r.GetModelInfo("one", "").SupportsWebSearch {
		t.Fatal("removed positive capability retained")
	}
	r.ApplyClientModelCapabilities("b", r.ClientRegistrationEpoch("b"), func(_ string, info *ModelInfo) {
		info.SupportsWebSearch = true
		info.NativeCapabilities = &NativeCapabilities{WebSearch: &yes}
	})
	if v := r.GetResponsesWebSearchCapability("one"); v == nil || !*v {
		t.Fatal("capability mutation not published")
	}
	r.RegisterClient("b", "gemini", []*ModelInfo{{ID: "one", NativeCapabilities: &NativeCapabilities{WebSearch: &yes}}})
	if v := r.GetResponsesWebSearchCapability("one"); v == nil || *v {
		t.Fatal("provider migration retained old capability")
	}
}

func TestPoolRegistryAvailableCacheExpiresWithoutWriter(t *testing.T) {
	r := newTestModelRegistry()
	r.EnablePoolReads()
	r.RegisterClient("a", "codex", []*ModelInfo{{ID: "model"}})
	now := time.Now()
	r.SetModelQuotaExceededUntil("a", "model", now.Add(time.Minute))
	r.poolAvailableModels("openai", now)
	first, _ := r.poolAvailableCache.Load("openai")
	r.poolAvailableModels("openai", now.Add(time.Second))
	second, _ := r.poolAvailableCache.Load("openai")
	if first != second {
		t.Fatal("unchanged model listing was rebuilt")
	}
	r.poolAvailableModels("openai", now.Add(2*time.Minute))
	third, _ := r.poolAvailableCache.Load("openai")
	if third == second {
		t.Fatal("quota deadline did not expire cache")
	}
	r.SuspendClientModel("a", "model", "disabled")
	if got := r.poolAvailableModels("openai", now.Add(2*time.Minute)); len(got) != 0 {
		t.Fatal("cache ignored new suspension")
	}
}
