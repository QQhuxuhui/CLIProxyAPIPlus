package cliproxy

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type serviceTestPluginExecutor struct{}
type serviceTestSDKExecutor struct{ serviceTestPluginExecutor }

func (serviceTestSDKExecutor) Identifier() string { return "sdk-provider" }

func (serviceTestPluginExecutor) Identifier() string {
	return "plugin-provider"
}

func (serviceTestPluginExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (serviceTestPluginExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (serviceTestPluginExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (serviceTestPluginExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (serviceTestPluginExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestRegisterAvailableExecutors(t *testing.T) {
	oldRegisterPluginExecutors := registerPluginExecutors
	pluginRegisterCalls := 0
	var expectedPluginHost *pluginhost.Host
	var expectedManager *coreauth.Manager
	registerPluginExecutors = func(host *pluginhost.Host, manager *coreauth.Manager) {
		pluginRegisterCalls++
		if host != expectedPluginHost {
			t.Fatalf("plugin executor registration host = %p, want %p", host, expectedPluginHost)
		}
		if manager != expectedManager {
			t.Fatalf("plugin executor registration manager = %p, want %p", manager, expectedManager)
		}
		manager.RegisterExecutor(serviceTestPluginExecutor{})
	}
	t.Cleanup(func() {
		registerPluginExecutors = oldRegisterPluginExecutors
	})

	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
		pluginHost:  pluginhost.New(),
	}
	expectedPluginHost = service.pluginHost
	expectedManager = service.coreManager
	service.ensureWebsocketGateway()

	service.registerAvailableExecutors(nil, executorRegistrationOptions{
		includeBaseline: true,
		includePlugins:  true,
	})

	if pluginRegisterCalls != 1 {
		t.Fatalf("plugin executor registration calls = %d, want 1", pluginRegisterCalls)
	}

	providers := []string{
		"codex",
		"claude",
		"gemini",
		"gemini-interactions",
		"vertex",
		"aistudio",
		"antigravity",
		"kimi",
		"xai",
		"openai-compatibility",
		"plugin-provider",
	}
	for _, provider := range providers {
		resolved, ok := service.coreManager.Executor(provider)
		if !ok || resolved == nil {
			t.Fatalf("expected executor for provider %s after registration", provider)
		}
	}

	resolved, _ := service.coreManager.Executor("plugin-provider")
	if _, isPlugin := resolved.(serviceTestPluginExecutor); !isPlugin {
		t.Fatalf("executor type = %T, want serviceTestPluginExecutor", resolved)
	}
}

func TestSyncPluginModelRuntimePreservesSDKExecutorUnlessForced(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	custom := serviceTestSDKExecutor{}
	manager.RegisterExecutor(custom)
	auth := &coreauth.Auth{ID: "private-auth", Provider: custom.Identifier()}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager, pluginHost: pluginhost.New()}

	service.syncPluginModelRuntime(context.Background())
	got, ok := manager.Executor(custom.Identifier())
	if !ok || got != custom {
		t.Fatalf("plugin model sync replaced SDK executor with %T", got)
	}

	service.registerExecutorForAuth(auth, true)
	got, ok = manager.Executor(custom.Identifier())
	if !ok {
		t.Fatal("forced registration removed executor")
	}
	if _, replaced := got.(*runtimeexecutor.OpenAICompatExecutor); !replaced {
		t.Fatalf("forced registration kept %T, want *executor.OpenAICompatExecutor", got)
	}
}

func TestRegisterExecutorForAuth_OpenAICompatUsesNamespacedProviderKey(t *testing.T) {
	testCases := []struct {
		name  string
		auths []*coreauth.Auth
	}{
		{
			name: "native first",
			auths: []*coreauth.Auth{
				{ID: "native-kimi", Provider: "kimi"},
				openAICompatKimiAuth(),
			},
		},
		{
			name: "compat first",
			auths: []*coreauth.Auth{
				openAICompatKimiAuth(),
				{ID: "native-kimi", Provider: "kimi"},
			},
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			service := &Service{
				cfg:         &config.Config{},
				coreManager: coreauth.NewManager(nil, nil, nil),
			}

			service.registerExecutorsForAuths(tt.auths, true)

			nativeExecutor, okNative := service.coreManager.Executor("kimi")
			if !okNative {
				t.Fatal("expected native kimi executor")
			}
			if _, okKimi := nativeExecutor.(*runtimeexecutor.KimiExecutor); !okKimi {
				t.Fatalf("native executor type = %T, want *executor.KimiExecutor", nativeExecutor)
			}

			compatExecutor, okCompat := service.coreManager.Executor("openai-compatible-kimi")
			if !okCompat {
				t.Fatal("expected namespaced OpenAI-compatible executor")
			}
			if _, okOpenAICompat := compatExecutor.(*runtimeexecutor.OpenAICompatExecutor); !okOpenAICompat {
				t.Fatalf("compat executor type = %T, want *executor.OpenAICompatExecutor", compatExecutor)
			}
		})
	}
}

func openAICompatKimiAuth() *coreauth.Auth {
	return &coreauth.Auth{
		ID:       "compat-kimi",
		Provider: "openai-compatibility",
		Label:    "kimi",
		Attributes: map[string]string{
			"compat_name":  "kimi",
			"provider_key": "kimi",
		},
	}
}

func TestExecutorRegistrationKey_DedupesSameProvider(t *testing.T) {
	keyA, okA := executorRegistrationKey(&coreauth.Auth{ID: "a", Provider: "antigravity"})
	keyB, okB := executorRegistrationKey(&coreauth.Auth{ID: "b", Provider: "Antigravity"})
	if !okA || !okB || keyA != keyB {
		t.Fatalf("same-provider auths should share a key: %+v/%v %+v/%v", keyA, okA, keyB, okB)
	}
	if _, ok := executorRegistrationKey(&coreauth.Auth{ID: "d", Provider: "antigravity", Disabled: true}); ok {
		t.Fatal("disabled auth should not be deduplicated")
	}
	studioA, _ := executorRegistrationKey(&coreauth.Auth{ID: "s1", Provider: "aistudio"})
	studioB, _ := executorRegistrationKey(&coreauth.Auth{ID: "s2", Provider: "aistudio"})
	if studioA == studioB {
		t.Fatal("aistudio auths bind per ID and must not share a key")
	}
	bare, _ := executorRegistrationKey(&coreauth.Auth{ID: "x1", Provider: "custom", Attributes: map[string]string{}})
	withURL, _ := executorRegistrationKey(&coreauth.Auth{ID: "x2", Provider: "custom", Attributes: map[string]string{"base_url": "https://example.com"}})
	if bare == withURL {
		t.Fatal("base_url presence changes the binding decision and must be part of the key")
	}
}

func TestRegisterExecutorsForAuths_ManyAuthsBindEachProvider(t *testing.T) {
	auths := make([]*coreauth.Auth, 0, 2001)
	for i := 0; i < 1000; i++ {
		auths = append(auths, &coreauth.Auth{ID: "ag-" + strconv.Itoa(i), Provider: "antigravity"})
		auths = append(auths, &coreauth.Auth{ID: "cx-" + strconv.Itoa(i), Provider: "codex"})
	}
	auths = append(auths, &coreauth.Auth{ID: "native-kimi", Provider: "kimi"})
	service := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}

	service.registerExecutorsForAuths(auths, true)

	for _, provider := range []string{"antigravity", "codex", "kimi"} {
		if _, ok := service.coreManager.Executor(provider); !ok {
			t.Fatalf("expected %s executor to be registered", provider)
		}
	}
}

func TestRegisterExecutorsForAuths_CompatLabels(t *testing.T) {
	for _, forceReplace := range []bool{false, true} {
		t.Run(strconv.FormatBool(forceReplace), func(t *testing.T) {
			service := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}
			service.registerExecutorsForAuths([]*coreauth.Auth{
				{ID: "a", Provider: "openai-compatibility", Label: "foo"},
				{ID: "b", Provider: "openai-compatibility", Label: "bar"},
			}, forceReplace)
			for _, provider := range []string{"openai-compatible-foo", "openai-compatible-bar"} {
				if _, ok := service.coreManager.Executor(provider); !ok {
					t.Errorf("missing executor %s", provider)
				}
			}
		})
	}
}

func TestRegisterExecutorsForAuths_InterleavedBindings(t *testing.T) {
	oldHasAuthProvider := pluginHostHasAuthProvider
	pluginHostHasAuthProvider = func(host *pluginhost.Host, provider string) bool {
		return host != nil && provider == "plugin-provider"
	}
	t.Cleanup(func() { pluginHostHasAuthProvider = oldHasAuthProvider })

	for _, forceReplace := range []bool{false, true} {
		t.Run(strconv.FormatBool(forceReplace), func(t *testing.T) {
			service := &Service{
				cfg:         &config.Config{},
				coreManager: coreauth.NewManager(nil, nil, nil),
				pluginHost:  pluginhost.New(),
			}
			service.registerExecutorsForAuths([]*coreauth.Auth{
				{ID: "a", Provider: "plugin-provider", Attributes: map[string]string{"compat_name": "shared"}},
				{ID: "b", Provider: "other-provider", Attributes: map[string]string{"compat_name": "shared"}},
				{ID: "c", Provider: "plugin-provider", Attributes: map[string]string{"compat_name": "shared"}},
			}, forceReplace)
			got, ok := service.coreManager.Executor("openai-compatible-shared")
			if !ok || !pluginhost.IsPluginRefreshCompatExecutor(got) {
				t.Fatalf("last auth should restore plugin refresh wrapper; got %T", got)
			}
		})
	}
}
