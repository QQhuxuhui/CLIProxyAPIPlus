# Kiro Cache Tokens 修复计划

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 修复 Kiro 渠道的缓存 token 数据丢失问题，使 `cache_read_input_tokens` 和 `cache_creation_input_tokens` 正确传递到下游（new-api 等）。

**Architecture:** 分两层修复：
1. Executor 层 — 将 Kiro 返回的 `cacheReadInputTokens` / `cacheWriteInputTokens` 正确映射到 `usage.Detail.CachedTokens`
2. Translator 层 — 在输出的 Claude 格式 JSON 中添加 `cache_read_input_tokens` / `cache_creation_input_tokens` 字段

**Tech Stack:** Go, gjson/sjson, SSE streaming

---

## Task 1: Executor 层 — parseEventStream 中设置 CachedTokens

**Files:**
- Modify: `internal/runtime/executor/kiro_executor.go:1992-2001`

**修改内容：**

在 `cacheReadInputTokens` 解析处（第 1993 行附近），增加 `usageInfo.CachedTokens` 赋值。
同时解析 `cacheWriteInputTokens`。

**原代码（第 1992-2001 行）：**
```go
// cacheReadInputTokens - tokens read from cache
if cacheReadTokens, ok := tokenUsage["cacheReadInputTokens"].(float64); ok {
    // Add to input tokens if we have uncached tokens, otherwise use as input
    if usageInfo.InputTokens > 0 {
        usageInfo.InputTokens += int64(cacheReadTokens)
    } else {
        usageInfo.InputTokens = int64(cacheReadTokens)
    }
    log.Debugf("kiro: parseEventStream found cacheReadInputTokens in tokenUsage: %d", int64(cacheReadTokens))
}
```

**新代码：**
```go
// cacheReadInputTokens - tokens read from cache
if cacheReadTokens, ok := tokenUsage["cacheReadInputTokens"].(float64); ok {
    usageInfo.CachedTokens = int64(cacheReadTokens)
    // Add to input tokens if we have uncached tokens, otherwise use as input
    if usageInfo.InputTokens > 0 {
        usageInfo.InputTokens += int64(cacheReadTokens)
    } else {
        usageInfo.InputTokens = int64(cacheReadTokens)
    }
    log.Debugf("kiro: parseEventStream found cacheReadInputTokens in tokenUsage: %d", int64(cacheReadTokens))
}
// cacheWriteInputTokens - tokens written to cache (first request)
if cacheWriteTokens, ok := tokenUsage["cacheWriteInputTokens"].(float64); ok {
    if usageInfo.CachedTokens == 0 {
        usageInfo.CachedTokens = int64(cacheWriteTokens)
    }
    log.Debugf("kiro: parseEventStream found cacheWriteInputTokens in tokenUsage: %d", int64(cacheWriteTokens))
}
```

---

## Task 2: Executor 层 — streamToChannel 中设置 CachedTokens

**Files:**
- Modify: `internal/runtime/executor/kiro_executor.go:3412-3422`

**修改内容：**

与 Task 1 相同的逻辑，应用到 `streamToChannel` 函数中的 `totalUsage`。

**原代码（第 3412-3422 行）：**
```go
// cacheReadInputTokens - tokens read from cache
if cacheReadTokens, ok := tokenUsage["cacheReadInputTokens"].(float64); ok {
    // Add to input tokens if we have uncached tokens, otherwise use as input
    if totalUsage.InputTokens > 0 {
        totalUsage.InputTokens += int64(cacheReadTokens)
    } else {
        totalUsage.InputTokens = int64(cacheReadTokens)
    }
    hasUpstreamUsage = true
    log.Debugf("kiro: streamToChannel found cacheReadInputTokens in tokenUsage: %d", int64(cacheReadTokens))
}
```

**新代码：**
```go
// cacheReadInputTokens - tokens read from cache
if cacheReadTokens, ok := tokenUsage["cacheReadInputTokens"].(float64); ok {
    totalUsage.CachedTokens = int64(cacheReadTokens)
    // Add to input tokens if we have uncached tokens, otherwise use as input
    if totalUsage.InputTokens > 0 {
        totalUsage.InputTokens += int64(cacheReadTokens)
    } else {
        totalUsage.InputTokens = int64(cacheReadTokens)
    }
    hasUpstreamUsage = true
    log.Debugf("kiro: streamToChannel found cacheReadInputTokens in tokenUsage: %d", int64(cacheReadTokens))
}
// cacheWriteInputTokens - tokens written to cache (first request)
if cacheWriteTokens, ok := tokenUsage["cacheWriteInputTokens"].(float64); ok {
    if totalUsage.CachedTokens == 0 {
        totalUsage.CachedTokens = int64(cacheWriteTokens)
    }
    hasUpstreamUsage = true
    log.Debugf("kiro: streamToChannel found cacheWriteInputTokens in tokenUsage: %d", int64(cacheWriteTokens))
}
```

---

## Task 3: Translator 层 — BuildClaudeResponse 添加缓存字段

**Files:**
- Modify: `internal/translator/kiro/claude/kiro_claude_response.go:123-126`

**原代码：**
```go
"usage": map[string]interface{}{
    "input_tokens":  usageInfo.InputTokens,
    "output_tokens": usageInfo.OutputTokens,
},
```

**新代码：**
```go
"usage": map[string]interface{}{
    "input_tokens":                usageInfo.InputTokens,
    "output_tokens":               usageInfo.OutputTokens,
    "cache_read_input_tokens":     usageInfo.CachedTokens,
    "cache_creation_input_tokens": 0,
},
```

---

## Task 4: Translator 层 — BuildClaudeMessageDeltaEvent 添加缓存字段

**Files:**
- Modify: `internal/translator/kiro/claude/kiro_claude_stream.go:120-123`

**原代码：**
```go
"usage": map[string]interface{}{
    "input_tokens":  usageInfo.InputTokens,
    "output_tokens": usageInfo.OutputTokens,
},
```

**新代码：**
```go
"usage": map[string]interface{}{
    "input_tokens":                usageInfo.InputTokens,
    "output_tokens":               usageInfo.OutputTokens,
    "cache_read_input_tokens":     usageInfo.CachedTokens,
    "cache_creation_input_tokens": 0,
},
```

---

## Task 5: Translator 层 — BuildClaudeMessageStartEvent 添加缓存字段

**Files:**
- Modify: `internal/translator/kiro/claude/kiro_claude_stream.go:25`

**原代码：**
```go
"usage": map[string]interface{}{"input_tokens": inputTokens, "output_tokens": 0},
```

**新代码：**
```go
"usage": map[string]interface{}{"input_tokens": inputTokens, "output_tokens": 0, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0},
```

---

## Task 6: Translator 层 — BuildClaudePingEventWithUsage 添加缓存字段

**Files:**
- Modify: `internal/translator/kiro/claude/kiro_claude_stream.go:143-146`

**原代码：**
```go
"usage": map[string]interface{}{
    "input_tokens":  inputTokens,
    "output_tokens": outputTokens,
    "total_tokens":  inputTokens + outputTokens,
    "estimated":     true,
},
```

**新代码：**
```go
"usage": map[string]interface{}{
    "input_tokens":                inputTokens,
    "output_tokens":               outputTokens,
    "cache_read_input_tokens":     int64(0),
    "cache_creation_input_tokens": int64(0),
    "total_tokens":                inputTokens + outputTokens,
    "estimated":                   true,
},
```

---

## Task 7: 编译验证 & 提交

**Step 1:** `go build ./...`
**Step 2:** `go vet ./...`
**Step 3:** `go test ./internal/runtime/executor/... -run Cache -v`
**Step 4:** Commit

```bash
git add internal/runtime/executor/kiro_executor.go \
        internal/translator/kiro/claude/kiro_claude_response.go \
        internal/translator/kiro/claude/kiro_claude_stream.go
git commit -m "fix(kiro): propagate cache token counts to downstream usage output

Kiro AWS backend returns cacheReadInputTokens and cacheWriteInputTokens
in tokenUsage metadata, but these values were:
1. Not stored in usage.Detail.CachedTokens (always 0)
2. Not included in Claude-format response JSON output

This fix:
- Sets CachedTokens in both parseEventStream and streamToChannel
- Adds cache_read_input_tokens and cache_creation_input_tokens to all
  Claude-format response builders (response, message_delta, message_start, ping)
- Enables new-api and other downstream consumers to see cache hit data"
```
