package executor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// antigravitySafetyRejectedMessage 是下游可依赖的固定文案：sub2api 的错误透传规则
// 「内容审核拦截透传」按这句话匹配并把 400 原样放行给客户；改动它要同步改规则。
const antigravitySafetyRejectedMessage = "Your request was rejected by the upstream safety system"

// antigravityBlockedFinishReasons 列出「模型一个字都没生成」的 finishReason。
// 这些值配合空 parts 出现时，上游是用 2xx 包了一个拒答；MAX_TOKENS / STOP 不在其中。
var antigravityBlockedFinishReasons = map[string]struct{}{
	"SAFETY":                   {},
	"RECITATION":               {},
	"BLOCKLIST":                {},
	"PROHIBITED_CONTENT":       {},
	"SPII":                     {},
	"LANGUAGE":                 {},
	"IMAGE_SAFETY":             {},
	"IMAGE_PROHIBITED_CONTENT": {},
	"IMAGE_RECITATION":         {},
	"IMAGE_OTHER":              {},
	"OTHER":                    {},
}

// antigravityResponseRoot 兼容 Cloud Code Assist 的 {"response":{...}} 包装与裸 Gemini 响应。
func antigravityResponseRoot(payload []byte) gjson.Result {
	if len(payload) == 0 {
		return gjson.Result{}
	}
	if wrapped := gjson.GetBytes(payload, "response"); wrapped.Exists() && wrapped.IsObject() {
		return wrapped
	}
	return gjson.ParseBytes(payload)
}

// antigravityCandidateHasContent 报告候选里是否有任何生成内容（文本、图片、函数调用等）。
func antigravityCandidateHasContent(candidate gjson.Result) bool {
	found := false
	candidate.Get("content.parts").ForEach(func(_, part gjson.Result) bool {
		if strings.TrimSpace(part.Get("text").String()) != "" ||
			part.Get("inlineData").Exists() ||
			part.Get("functionCall").Exists() ||
			part.Get("executableCode").Exists() ||
			part.Get("codeExecutionResult").Exists() ||
			part.Get("fileData").Exists() {
			found = true
			return false
		}
		return true
	})
	return found
}

// antigravityPayloadHasContent 报告一个（流式或非流式）载荷是否带任何生成内容。
func antigravityPayloadHasContent(payload []byte) bool {
	root := antigravityResponseRoot(payload)
	found := false
	root.Get("candidates").ForEach(func(_, c gjson.Result) bool {
		if antigravityCandidateHasContent(c) {
			found = true
			return false
		}
		return true
	})
	return found
}

// antigravityBlockedReason 判断一个 2xx 载荷是否是「被风控拦下、没有任何生成内容」：
//   - promptFeedback.blockReason 存在：提示词整条被拦；
//   - candidates[0].finishReason 属于拦截集合且 parts 为空：候选被拦。
//
// 仅看显式信号，不把「没有 candidates」当拦截（流式的 usage 尾块就没有 candidates）。
func antigravityBlockedReason(payload []byte) (reason string, blocked bool) {
	root := antigravityResponseRoot(payload)
	if !root.Exists() {
		return "", false
	}
	if br := strings.TrimSpace(root.Get("promptFeedback.blockReason").String()); br != "" {
		return "promptFeedback.blockReason=" + br, true
	}
	cands := root.Get("candidates")
	if !cands.IsArray() || len(cands.Array()) == 0 {
		return "", false
	}
	first := cands.Array()[0]
	fr := strings.ToUpper(strings.TrimSpace(first.Get("finishReason").String()))
	if _, ok := antigravityBlockedFinishReasons[fr]; !ok {
		return "", false
	}
	if antigravityCandidateHasContent(first) {
		return "", false
	}
	return "finishReason=" + fr, true
}

// antigravityNonStreamBlockedReason 在非流式完整体上比 antigravityBlockedReason 多一条：
// 一个 2xx 的 generateContent 完整体既没有 candidates 也没有 blockReason，同样是空拒答
// （下游会记成 0 token 的成功），按 EMPTY_CANDIDATES 处理。
func antigravityNonStreamBlockedReason(payload []byte) (reason string, blocked bool) {
	if reason, blocked = antigravityBlockedReason(payload); blocked {
		return reason, true
	}
	root := antigravityResponseRoot(payload)
	if !root.Exists() || !root.IsObject() {
		return "", false
	}
	cands := root.Get("candidates")
	if !cands.Exists() || (cands.IsArray() && len(cands.Array()) == 0) {
		return "EMPTY_CANDIDATES", true
	}
	return "", false
}

// newAntigravityBlockedStatusErr 构造给下游的明确报错：400 + Google 风格错误体。
// status=INVALID_ARGUMENT 让 conductor 的 isRequestInvalidError 判定为请求级错误，
// 不会换号重试（拦截是确定性的，换号只会白烧配额）；type=content_filter 供客户端程序识别。
func newAntigravityBlockedStatusErr(reason string) statusErr {
	msg := fmt.Sprintf("%s (%s)", antigravitySafetyRejectedMessage, reason)
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":    http.StatusBadRequest,
			"message": msg,
			"status":  "INVALID_ARGUMENT",
			"type":    "content_filter",
			"reason":  reason,
		},
	})
	return statusErr{code: http.StatusBadRequest, msg: string(body)}
}

// antigravityBodyPreview 截断响应体供日志观察，避免把整段 base64 写进日志。
func antigravityBodyPreview(body []byte) string {
	const limit = 512
	s := strings.TrimSpace(string(body))
	if len(s) > limit {
		return s[:limit] + "...(truncated)"
	}
	return s
}
