package cliproxy

import (
	"context"
	"fmt"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexSpeedProbeRoundSkipsRejectedHeadOfQueue(t *testing.T) {
	tracker := coreauth.NewCodexSpeedTracker(nil)
	var infos []coreauth.CodexSpeedAuthInfo
	for i := 0; i < 10; i++ {
		infos = append(infos, coreauth.CodexSpeedAuthInfo{ID: fmt.Sprintf("a%02d", i), Schedulable: true, QuotaUsed: float64(i)})
	}
	var launched []string
	svc := &codexSpeedService{tracker: tracker}
	svc.acquire = func(id, model string) *coreauth.Auth {
		if id < "a05" { // the five accounts at the head of the queue are always rejected
			return nil
		}
		return &coreauth.Auth{ID: id}
	}
	svc.launch = func(ctx context.Context, auth *coreauth.Auth, model string) {
		launched = append(launched, auth.ID)
		svc.inflight.Add(-1)
	}
	if n := svc.probeRound(context.Background(), infos, 2, "m"); n != 2 || len(launched) != 2 || launched[0] != "a05" || launched[1] != "a06" {
		t.Fatalf("rejected accounts must not block later candidates: started=%d launched=%v", n, launched)
	}
	// Rejected accounts keep their probe interval: they are still candidates next second.
	if got := tracker.ProbeCandidates(infos, 10); len(got) == 0 || got[0] != "a00" {
		t.Fatalf("rejection must not spend the interval: %v", got)
	}
}
