package executor

import "testing"

// 锁定：版本错误检测能匹配 Google 的固定错误文本，且不误伤正常响应。
func TestAntigravityResponseHasVersionError(t *testing.T) {
	versionErr := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"This version of Antigravity is no longer supported. Please upgrade to receive the latest features."}]},"finishReason":"STOP"}]}`)
	if !antigravityResponseHasVersionError(versionErr) {
		t.Fatal("应识别出版本错误")
	}

	normal := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hi! How can I help you today?"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":8}}`)
	if antigravityResponseHasVersionError(normal) {
		t.Fatal("正常响应不应被误判为版本错误")
	}

	if antigravityResponseHasVersionError(nil) || antigravityResponseHasVersionError([]byte("")) {
		t.Fatal("空响应应返回 false")
	}
}
