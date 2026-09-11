package executor

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// 锁定：风控拦截的 2xx 载荷被识别出来，正常/半正常载荷不被误伤。
func TestAntigravityBlockedReason(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
		blocked bool
	}{
		{"prompt blocked (wrapped)", `{"response":{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT","blockReasonMessage":"x"},"usageMetadata":{"promptTokenCount":12}}}`, "promptFeedback.blockReason=PROHIBITED_CONTENT", true},
		{"prompt blocked (bare)", `{"promptFeedback":{"blockReason":"SAFETY"}}`, "promptFeedback.blockReason=SAFETY", true},
		{"candidate SAFETY no parts (wrapped)", `{"response":{"candidates":[{"finishReason":"SAFETY","safetyRatings":[{"category":"HARM_CATEGORY_HATE_SPEECH","probability":"HIGH"}]}],"usageMetadata":{"promptTokenCount":3224,"totalTokenCount":3224}}}`, "finishReason=SAFETY", true},
		{"candidate RECITATION empty content", `{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"RECITATION"}]}`, "finishReason=RECITATION", true},
		{"candidate OTHER whitespace text only", `{"candidates":[{"content":{"role":"model","parts":[{"text":"  "}]},"finishReason":"OTHER"}]}`, "finishReason=OTHER", true},
		{"SAFETY but partial text present", `{"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]},"finishReason":"SAFETY"}]}`, "", false},
		{"STOP with empty parts (terminal chunk)", `{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":10}}`, "", false},
		{"MAX_TOKENS with no parts (thinking ate budget)", `{"candidates":[{"finishReason":"MAX_TOKENS"}],"usageMetadata":{"thoughtsTokenCount":1024}}`, "", false},
		{"normal text", `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"Hi"}]},"finishReason":"STOP"}]}}`, "", false},
		{"function call only", `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{}}}]},"finishReason":"STOP"}]}`, "", false},
		{"usage-only stream tail (no candidates)", `{"response":{"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":5}}}`, "", false},
		{"empty", ``, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, blocked := antigravityBlockedReason([]byte(tc.payload))
			if blocked != tc.blocked || got != tc.want {
				t.Fatalf("got (%q,%v) want (%q,%v)", got, blocked, tc.want, tc.blocked)
			}
		})
	}
}

// 非流式完整体：没有 candidates 也没有 blockReason 的 2xx 同样按空拒答处理；
// 但流式检测器不这么做（usage 尾块没有 candidates）。
func TestAntigravityNonStreamBlockedReason_EmptyCandidates(t *testing.T) {
	for _, payload := range []string{
		`{"response":{"usageMetadata":{"promptTokenCount":3224,"totalTokenCount":3224}}}`,
		`{"response":{"candidates":[],"usageMetadata":{"promptTokenCount":1}}}`,
		`{"candidates":[]}`,
	} {
		reason, blocked := antigravityNonStreamBlockedReason([]byte(payload))
		if !blocked || reason != "EMPTY_CANDIDATES" {
			t.Fatalf("%s: got (%q,%v)", payload, reason, blocked)
		}
		if _, streamBlocked := antigravityBlockedReason([]byte(payload)); streamBlocked {
			t.Fatalf("%s: stream detector must not flag missing candidates", payload)
		}
	}
	if _, blocked := antigravityNonStreamBlockedReason([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}}`)); blocked {
		t.Fatal("normal non-stream body must not be flagged")
	}
	if _, blocked := antigravityNonStreamBlockedReason([]byte(`not json`)); blocked {
		t.Fatal("non-object body must not be flagged")
	}
}

func TestAntigravityPayloadHasContent(t *testing.T) {
	if !antigravityPayloadHasContent([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"a"}]}}]}}`)) {
		t.Fatal("text part should count as content")
	}
	if !antigravityPayloadHasContent([]byte(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"AAAA"}}]}}]}`)) {
		t.Fatal("inlineData should count as content")
	}
	if antigravityPayloadHasContent([]byte(`{"candidates":[{"content":{"parts":[{"thought":true,"text":""}]}}]}`)) {
		t.Fatal("empty text must not count as content")
	}
	if antigravityPayloadHasContent([]byte(`{"response":{"usageMetadata":{"promptTokenCount":1}}}`)) {
		t.Fatal("usage-only payload has no content")
	}
}

// 下游契约：400、合法 JSON 错误体、带 sub2api 透传规则依赖的固定文案、
// status=INVALID_ARGUMENT（conductor.isRequestInvalidError 据此不换号重试）、type=content_filter。
func TestNewAntigravityBlockedStatusErr(t *testing.T) {
	err := newAntigravityBlockedStatusErr("finishReason=PROHIBITED_CONTENT")
	if err.StatusCode() != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", err.StatusCode())
	}
	body := err.Error()
	if !json.Valid([]byte(body)) {
		t.Fatalf("body must be valid JSON so handlers pass it through verbatim: %s", body)
	}
	var parsed struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
			Type    string `json:"type"`
			Reason  string `json:"reason"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal([]byte(body), &parsed); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if parsed.Error.Code != 400 || parsed.Error.Status != "INVALID_ARGUMENT" || parsed.Error.Type != "content_filter" || parsed.Error.Reason != "finishReason=PROHIBITED_CONTENT" {
		t.Fatalf("unexpected error shape: %+v", parsed.Error)
	}
	if !strings.HasPrefix(parsed.Error.Message, antigravitySafetyRejectedMessage) {
		t.Fatalf("message must start with the fixed downstream marker: %q", parsed.Error.Message)
	}
	if !strings.Contains(body, "INVALID_ARGUMENT") {
		t.Fatal("body must contain INVALID_ARGUMENT for request-scoped classification")
	}
}
