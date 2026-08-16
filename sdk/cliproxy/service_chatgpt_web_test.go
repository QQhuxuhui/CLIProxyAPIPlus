package cliproxy

import (
	"testing"

	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestEnsureExecutorsForAuthChatGPTWeb(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "chatgpt-web-1.json",
		Provider: "chatgpt-web",
		Status:   coreauth.StatusActive,
	}

	service.ensureExecutorsForAuth(auth)

	resolved, ok := service.coreManager.Executor("chatgpt-web")
	if !ok || resolved == nil {
		t.Fatal("expected ChatGPT Web executor after auth bind")
	}
	if _, okType := resolved.(*runtimeexecutor.ChatGPTWebExecutor); !okType {
		t.Fatalf("executor type = %T, want *executor.ChatGPTWebExecutor", resolved)
	}
}
