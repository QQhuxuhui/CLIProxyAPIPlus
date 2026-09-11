package auth

import "testing"

func TestAuthKind(t *testing.T) {
	tests := []struct {
		name string
		auth *Auth
		want string
	}{
		{
			name: "explicit api key attribute",
			auth: &Auth{Attributes: map[string]string{AttributeAuthKind: "api_key"}},
			want: AuthKindAPIKey,
		},
		{
			name: "explicit oauth attribute wins over api key fallback",
			auth: &Auth{Attributes: map[string]string{AttributeAuthKind: "oauth", AttributeAPIKey: "k"}},
			want: AuthKindOAuth,
		},
		{
			name: "explicit oauth metadata",
			auth: &Auth{Metadata: map[string]any{AttributeAuthKind: "oauth"}},
			want: AuthKindOAuth,
		},
		{
			name: "legacy api key attribute",
			auth: &Auth{Attributes: map[string]string{AttributeAPIKey: "k"}},
			want: AuthKindAPIKey,
		},
		{
			name: "legacy oauth metadata",
			auth: &Auth{Metadata: map[string]any{"access_token": "token"}},
			want: AuthKindOAuth,
		},
		{
			name: "unknown metadata shape",
			auth: &Auth{Metadata: map[string]any{"type": "test"}},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.auth.AuthKind(); got != tt.want {
				t.Fatalf("AuthKind() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAuthSourceKind(t *testing.T) {
	tests := []struct {
		name string
		auth *Auth
		want string
	}{
		{
			name: "runtime only memory",
			auth: &Auth{Attributes: map[string]string{AttributeRuntimeOnly: "true", AttributeSourceBackend: AuthSourcePostgres}},
			want: AuthSourceMemory,
		},
		{
			name: "backend postgres",
			auth: &Auth{Attributes: map[string]string{AttributeSourceBackend: "postgresql", AttributePath: "/tmp/auth.json"}},
			want: AuthSourcePostgres,
		},
		{
			name: "backend object store",
			auth: &Auth{Attributes: map[string]string{AttributeSourceBackend: "object-store", AttributePath: "/tmp/auth.json"}},
			want: AuthSourceObjectStore,
		},
		{
			name: "config source",
			auth: &Auth{Attributes: map[string]string{AttributeSource: "config:codex[abc]"}},
			want: AuthSourceConfig,
		},
		{
			name: "path source",
			auth: &Auth{Attributes: map[string]string{AttributeSource: "/tmp/auth.json"}},
			want: AuthSourceFile,
		},
		{
			name: "path attribute",
			auth: &Auth{Attributes: map[string]string{AttributePath: "/tmp/auth.json"}},
			want: AuthSourceFile,
		},
		{
			name: "filename fallback",
			auth: &Auth{FileName: "codex.json"},
			want: AuthSourceFile,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.auth.AuthSourceKind(); got != tt.want {
				t.Fatalf("AuthSourceKind() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAccountInfoUsesAuthKind(t *testing.T) {
	apiKeyAuth := &Auth{Attributes: map[string]string{AttributeAuthKind: "api-key", AttributeAPIKey: "k"}}
	kind, value := apiKeyAuth.AccountInfo()
	if kind != "api_key" || value != "k" {
		t.Fatalf("api key AccountInfo() = %q, %q", kind, value)
	}

	oauthAuth := &Auth{
		Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth, AttributeAPIKey: "k"},
		Metadata:   map[string]any{"email": "user@example.com"},
	}
	kind, value = oauthAuth.AccountInfo()
	if kind != "oauth" || value != "user@example.com" {
		t.Fatalf("oauth AccountInfo() = %q, %q", kind, value)
	}

	oauthWithoutEmail := &Auth{Metadata: map[string]any{"access_token": "token"}}
	kind, value = oauthWithoutEmail.AccountInfo()
	if kind != "oauth" || value != "" {
		t.Fatalf("oauth without email AccountInfo() = %q, %q", kind, value)
	}
}

// 风控拦截错误契约：executor 把 2xx 空拒答转成 400 + INVALID_ARGUMENT 的 Google 风格错误体，
// 这里锁定 conductor 把它判成「请求级错误」——不换号重试（拦截由内容决定，换号只会白烧配额）。
type contentFilterStatusErr struct{ body string }

func (e contentFilterStatusErr) Error() string   { return e.body }
func (e contentFilterStatusErr) StatusCode() int { return 400 }

func TestIsRequestInvalidError_ContentFilterBodyIsRequestScoped(t *testing.T) {
	err := contentFilterStatusErr{body: `{"error":{"code":400,"message":"Your request was rejected by the upstream safety system (finishReason=PROHIBITED_CONTENT)","status":"INVALID_ARGUMENT","type":"content_filter","reason":"finishReason=PROHIBITED_CONTENT"}}`}
	if !isRequestInvalidError(err) {
		t.Fatal("content_filter 400 with INVALID_ARGUMENT must be request-scoped (no auth rotation)")
	}
}
