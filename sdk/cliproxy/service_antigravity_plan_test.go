package cliproxy

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestConfigureAntigravityPlanStoreReplacesAuthDirectoryState(t *testing.T) {
	firstDir := t.TempDir()
	firstStore := coreauth.NewFileAntigravityPlanStore(firstDir)
	if errSave := firstStore.Save(context.Background(), map[string]coreauth.AntigravityPlanRecord{
		"first-auth": {PaidTierID: "pro", UpdatedAt: time.Now()},
	}); errSave != nil {
		t.Fatalf("save first plan snapshot: %v", errSave)
	}
	t.Cleanup(func() {
		_ = coreauth.ConfigureAntigravityPlanStore(context.Background(), nil)
	})

	service := &Service{}
	service.configureAntigravityPlanStore(context.Background(), &config.Config{AuthDir: firstDir})
	if record, ok := coreauth.GetAntigravityDisplayPlan("first-auth"); !ok || record.PaidTierID != "pro" {
		t.Fatalf("first display plan = %#v, %t; want pro", record, ok)
	}
	if _, ok := coreauth.GetAntigravityCreditsHint("first-auth"); ok {
		t.Fatal("restored display plan created a credits hint")
	}

	service.configureAntigravityPlanStore(context.Background(), &config.Config{AuthDir: t.TempDir()})
	if _, ok := coreauth.GetAntigravityDisplayPlan("first-auth"); ok {
		t.Fatal("auth directory replacement retained stale display plan")
	}
}

func TestConfigureAntigravityPlanStoreClearsHomeMode(t *testing.T) {
	t.Cleanup(func() {
		_ = coreauth.ConfigureAntigravityPlanStore(context.Background(), nil)
	})
	service := &Service{}

	coreauth.SetAntigravityDisplayPlan("home-clear-auth", "pro", time.Now())
	homeConfig := &config.Config{}
	homeConfig.Home.Enabled = true
	service.configureAntigravityPlanStore(context.Background(), homeConfig)
	if _, ok := coreauth.GetAntigravityDisplayPlan("home-clear-auth"); ok {
		t.Fatal("Home mode retained display plan")
	}
}

func TestServiceShutdownClearsAntigravityDisplayPlans(t *testing.T) {
	const childEnv = "CLIPROXY_TEST_PLAN_SHUTDOWN_CHILD"
	if os.Getenv(childEnv) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestServiceShutdownClearsAntigravityDisplayPlans$")
		cmd.Env = append(os.Environ(), childEnv+"=1")
		output, errRun := cmd.CombinedOutput()
		if errRun != nil {
			t.Fatalf("shutdown child test failed: %v\n%s", errRun, output)
		}
		return
	}

	_ = coreauth.ConfigureAntigravityPlanStore(context.Background(), nil)
	coreauth.SetAntigravityDisplayPlan("shutdown-clear-auth", "pro", time.Now())
	service := &Service{}
	if errShutdown := service.Shutdown(context.Background()); errShutdown != nil {
		t.Fatalf("Shutdown() returned error: %v", errShutdown)
	}
	if _, ok := coreauth.GetAntigravityDisplayPlan("shutdown-clear-auth"); ok {
		t.Fatal("service shutdown retained display plan")
	}
}
