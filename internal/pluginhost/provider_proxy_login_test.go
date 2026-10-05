package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPluginLoginUsesProviderProxy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile *pluginapi.HTTPWireProfile
	}{
		{"normal", nil},
		{"http1", &pluginapi.HTTPWireProfile{HTTP1Only: true}},
		{"ordered headers", &pluginapi.HTTPWireProfile{HeaderProfile: []string{"Host", "User-Agent"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var providerRequests, globalRequests atomic.Int32
			proxyHandler := func(counter *atomic.Int32, name string) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					counter.Add(1)
					if r.Method == http.MethodConnect {
						http.Error(w, "CONNECT probe", http.StatusBadGateway)
						return
					}
					_, _ = fmt.Fprint(w, name)
				}
			}
			providerProxy := httptest.NewServer(proxyHandler(&providerRequests, "provider"))
			defer providerProxy.Close()
			globalProxy := httptest.NewServer(proxyHandler(&globalRequests, "global"))
			defer globalProxy.Close()
			request := func(ctx context.Context, client pluginapi.HostHTTPClient) error {
				resp, err := client.Do(ctx, pluginapi.HTTPRequest{URL: "http://login.invalid/token", WireProfile: tc.profile})
				if err != nil {
					return err
				}
				if string(resp.Body) != "provider" {
					return fmt.Errorf("login used %q proxy, want provider", resp.Body)
				}
				// A rejected CONNECT proves that the TLS path selected the same proxy without contacting an upstream.
				_, err = client.Do(ctx, pluginapi.HTTPRequest{URL: "https://login.invalid/token", WireProfile: tc.profile})
				if err == nil {
					return fmt.Errorf("expected CONNECT probe rejection")
				}
				return nil
			}
			host := newHostWithRecords(capabilityRecord{id: "login-plugin", plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{AuthProvider: fakeAuthProvider{
				identifier: "plugin-provider",
				startLogin: func(ctx context.Context, req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
					return pluginapi.AuthLoginStartResponse{}, request(ctx, req.HTTPClient)
				},
				pollLogin: func(ctx context.Context, req pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
					return pluginapi.AuthLoginPollResponse{}, request(ctx, req.HTTPClient)
				},
			}}}})
			host.runtimeConfig = &config.Config{SDKConfig: config.SDKConfig{ProxyURL: globalProxy.URL, ProviderProxyURLs: map[string]string{"plugin-provider": providerProxy.URL}}}
			if _, handled, err := host.StartLogin(context.Background(), "plugin-provider", ""); err != nil || !handled {
				t.Fatalf("StartLogin handled=%v error=%v", handled, err)
			}
			if _, handled, err := host.PollLogin(context.Background(), "plugin-provider", "state"); err != nil || !handled {
				t.Fatalf("PollLogin handled=%v error=%v", handled, err)
			}
			if providerRequests.Load() != 4 || globalRequests.Load() != 0 {
				t.Fatalf("provider requests=%d global requests=%d", providerRequests.Load(), globalRequests.Load())
			}
		})
	}
}

func TestRPCPluginLoginUsesProviderProxy(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			var providerRequests, globalRequests atomic.Int32
			providerProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				providerRequests.Add(1)
				_, _ = fmt.Fprint(w, "provider")
			}))
			defer providerProxy.Close()
			globalProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { globalRequests.Add(1); _, _ = fmt.Fprint(w, "global") }))
			defer globalProxy.Close()
			host := New()
			host.runtimeConfig = &config.Config{SDKConfig: config.SDKConfig{ProxyURL: globalProxy.URL, ProviderProxyURLs: map[string]string{"plugin-provider": providerProxy.URL}}}
			adapter := &rpcPluginAdapter{id: "login-plugin", host: host, client: &loginCallbackPluginClient{host: host, stream: stream}}
			client := host.newHTTPClient(nil, "plugin-provider")
			if _, err := adapter.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{Provider: "plugin-provider", HTTPClient: client}); err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.PollLogin(context.Background(), pluginapi.AuthLoginPollRequest{Provider: "plugin-provider", HTTPClient: client}); err != nil {
				t.Fatal(err)
			}
			if providerRequests.Load() != 2 || globalRequests.Load() != 0 {
				t.Fatalf("provider requests=%d global requests=%d", providerRequests.Load(), globalRequests.Load())
			}
		})
	}
}

type loginCallbackPluginClient struct {
	host   *Host
	stream bool
}

func (c *loginCallbackPluginClient) Shutdown() {}
func (c *loginCallbackPluginClient) Call(_ context.Context, method string, request []byte) ([]byte, error) {
	if method != pluginabi.MethodAuthLoginStart && method != pluginabi.MethodAuthLoginPoll {
		return nil, fmt.Errorf("unexpected method %s", method)
	}
	var req struct {
		HostCallbackID string `json:"host_callback_id"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	if req.HostCallbackID == "" {
		return nil, fmt.Errorf("missing callback ID")
	}
	raw, err := json.Marshal(rpcHostHTTPRequest{HostCallbackID: req.HostCallbackID, URL: "http://login.invalid/token"})
	if err != nil {
		return nil, err
	}
	callbackMethod := pluginabi.MethodHostHTTPDo
	if c.stream {
		callbackMethod = pluginabi.MethodHostHTTPDoStream
	}
	response, err := c.host.callFromPlugin(context.Background(), callbackMethod, raw)
	if err != nil {
		return nil, err
	}
	if c.stream {
		stream, err := decodeRPCEnvelope[rpcHostHTTPStreamResponse](response)
		if err != nil {
			return nil, err
		}
		closeReq, err := json.Marshal(rpcHostHTTPStreamCloseRequest{StreamID: stream.StreamID})
		if err != nil {
			return nil, err
		}
		if _, err = c.host.callFromPlugin(context.Background(), pluginabi.MethodHostHTTPStreamClose, closeReq); err != nil {
			return nil, err
		}
	}
	return marshalRPCResult(rpcEmptyResponse{})
}
