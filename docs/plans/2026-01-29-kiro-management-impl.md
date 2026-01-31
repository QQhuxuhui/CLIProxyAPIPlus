# Kiro 账号管理功能实现计划

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 在 management API 中新增 Kiro 用量查询接口，支持前端按需获取 Kiro 账号的用量/配额信息。

**Architecture:** 新增一个 GET 接口 `/v0/management/kiro-usage`，通过 `auth_index` 参数定位 Kiro 认证对象，调用现有的 `KiroAuth.GetUsageLimits` 方法获取用量信息，格式化后返回给前端。

**Tech Stack:** Go, Gin framework, 复用现有的 kiroauth 包

---

## Task 1: 创建 Kiro 用量接口处理器

**Files:**
- Create: `internal/api/handlers/management/kiro_usage.go`

**Step 1: 创建 kiro_usage.go 文件**

```go
package management

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/kiro"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// GetKiroUsage handles GET /v0/management/kiro-usage
// Returns usage/quota information for a specific Kiro auth by auth_index
func (h *Handler) GetKiroUsage(c *gin.Context) {
	// 1. Parse auth_index parameter
	authIndexStr := c.Query("auth_index")
	if authIndexStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   "auth_index parameter is required",
		})
		return
	}

	authIndex, err := strconv.Atoi(authIndexStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   "invalid auth_index: must be an integer",
		})
		return
	}

	// 2. Get auth object from manager
	if h.authManager == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error":   "auth manager not initialized",
		})
		return
	}

	auth := h.authManager.GetByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"error":   "auth not found",
		})
		return
	}

	// 3. Verify it's a Kiro type
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "kiro") {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   "not a kiro auth",
		})
		return
	}

	// 4. Extract credentials from auth object
	accessToken, profileArn := extractKiroCredentials(auth)
	if accessToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   "kiro access token not found",
		})
		return
	}

	// 5. Call GetUsageLimits
	kAuth := kiroauth.NewKiroAuth(h.cfg)
	tokenData := &kiroauth.KiroTokenData{
		AccessToken: accessToken,
		ProfileArn:  profileArn,
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	usageInfo, err := kAuth.GetUsageLimits(ctx, tokenData)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"error":   "failed to get usage info: " + err.Error(),
		})
		return
	}

	// 6. Format and return response
	var percentage float64
	if usageInfo.UsageLimit > 0 {
		percentage = (usageInfo.CurrentUsage / usageInfo.UsageLimit) * 100
	}

	// Parse next reset timestamp
	var daysUntilReset int
	var nextResetDate string
	if usageInfo.NextReset != "" {
		if ts, err := strconv.ParseFloat(usageInfo.NextReset, 64); err == nil && ts > 0 {
			resetTime := time.Unix(int64(ts), 0)
			daysUntilReset = int(time.Until(resetTime).Hours() / 24)
			nextResetDate = resetTime.Format(time.RFC3339)
		}
	}

	// Get email from auth
	email := extractKiroEmail(auth)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"email":        email,
			"subscription": usageInfo.SubscriptionTitle,
			"usage": gin.H{
				"current":    usageInfo.CurrentUsage,
				"limit":      usageInfo.UsageLimit,
				"percentage": percentage,
			},
			"reset": gin.H{
				"days_until": daysUntilReset,
				"next_date":  nextResetDate,
			},
		},
	})
}

// extractKiroCredentials extracts access token and profile ARN from auth object
func extractKiroCredentials(auth *coreauth.Auth) (accessToken, profileArn string) {
	if auth == nil {
		return "", ""
	}

	// Try Metadata first (wrapped format)
	if auth.Metadata != nil {
		if token, ok := auth.Metadata["access_token"].(string); ok && token != "" {
			accessToken = token
		}
		if arn, ok := auth.Metadata["profile_arn"].(string); ok && arn != "" {
			profileArn = arn
		}
	}

	// Try Attributes
	if accessToken == "" && auth.Attributes != nil {
		if token := auth.Attributes["access_token"]; token != "" {
			accessToken = token
		}
		if arn := auth.Attributes["profile_arn"]; arn != "" {
			profileArn = arn
		}
	}

	// Try camelCase format (AWS Builder ID format)
	if accessToken == "" && auth.Metadata != nil {
		if token, ok := auth.Metadata["accessToken"].(string); ok && token != "" {
			accessToken = token
		}
		if arn, ok := auth.Metadata["profileArn"].(string); ok && arn != "" {
			profileArn = arn
		}
	}

	return accessToken, profileArn
}

// extractKiroEmail extracts email from auth object
func extractKiroEmail(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}

	// Try Metadata
	if auth.Metadata != nil {
		if email, ok := auth.Metadata["email"].(string); ok && email != "" {
			return email
		}
	}

	// Try Attributes
	if auth.Attributes != nil {
		if email := auth.Attributes["email"]; email != "" {
			return email
		}
	}

	return ""
}
```

**Step 2: 验证文件语法**

Run: `cd /usr/src/workspace/github/QQhuxuhui/CLIProxyAPIPlus && go build ./internal/api/handlers/management/`
Expected: 编译成功，无错误

**Step 3: Commit**

```bash
git add internal/api/handlers/management/kiro_usage.go
git commit -m "feat(management): add kiro usage endpoint handler"
```

---

## Task 2: 注册路由

**Files:**
- Modify: `internal/api/server.go:649` (在 kiro-auth-url 路由后添加)

**Step 1: 添加路由注册**

在 `internal/api/server.go` 文件中，找到第 649 行：
```go
mgmt.GET("/kiro-auth-url", s.mgmt.RequestKiroToken)
```

在其后添加：
```go
mgmt.GET("/kiro-usage", s.mgmt.GetKiroUsage)
```

**Step 2: 验证编译**

Run: `cd /usr/src/workspace/github/QQhuxuhui/CLIProxyAPIPlus && go build ./...`
Expected: 编译成功，无错误

**Step 3: Commit**

```bash
git add internal/api/server.go
git commit -m "feat(api): register kiro-usage route"
```

---

## Task 3: 添加单元测试

**Files:**
- Create: `internal/api/handlers/management/kiro_usage_test.go`

**Step 1: 创建测试文件**

```go
package management

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestGetKiroUsage_MissingAuthIndex(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{}
	router := gin.New()
	router.GET("/kiro-usage", h.GetKiroUsage)

	req := httptest.NewRequest(http.MethodGet, "/kiro-usage", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestGetKiroUsage_InvalidAuthIndex(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{}
	router := gin.New()
	router.GET("/kiro-usage", h.GetKiroUsage)

	req := httptest.NewRequest(http.MethodGet, "/kiro-usage?auth_index=abc", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}
}

func TestGetKiroUsage_NilAuthManager(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := &Handler{authManager: nil}
	router := gin.New()
	router.GET("/kiro-usage", h.GetKiroUsage)

	req := httptest.NewRequest(http.MethodGet, "/kiro-usage?auth_index=0", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status %d, got %d", http.StatusInternalServerError, w.Code)
	}
}

func TestExtractKiroCredentials_Nil(t *testing.T) {
	token, arn := extractKiroCredentials(nil)
	if token != "" || arn != "" {
		t.Errorf("expected empty strings for nil auth")
	}
}

func TestExtractKiroCredentials_Metadata(t *testing.T) {
	auth := &coreauth.Auth{
		Metadata: map[string]any{
			"access_token": "test-token",
			"profile_arn":  "test-arn",
		},
	}
	token, arn := extractKiroCredentials(auth)
	if token != "test-token" {
		t.Errorf("expected token 'test-token', got '%s'", token)
	}
	if arn != "test-arn" {
		t.Errorf("expected arn 'test-arn', got '%s'", arn)
	}
}

func TestExtractKiroCredentials_CamelCase(t *testing.T) {
	auth := &coreauth.Auth{
		Metadata: map[string]any{
			"accessToken": "camel-token",
			"profileArn":  "camel-arn",
		},
	}
	token, arn := extractKiroCredentials(auth)
	if token != "camel-token" {
		t.Errorf("expected token 'camel-token', got '%s'", token)
	}
	if arn != "camel-arn" {
		t.Errorf("expected arn 'camel-arn', got '%s'", arn)
	}
}

func TestExtractKiroEmail_Metadata(t *testing.T) {
	auth := &coreauth.Auth{
		Metadata: map[string]any{
			"email": "test@example.com",
		},
	}
	email := extractKiroEmail(auth)
	if email != "test@example.com" {
		t.Errorf("expected email 'test@example.com', got '%s'", email)
	}
}

func TestExtractKiroEmail_Nil(t *testing.T) {
	email := extractKiroEmail(nil)
	if email != "" {
		t.Errorf("expected empty string for nil auth")
	}
}
```

**Step 2: 运行测试**

Run: `cd /usr/src/workspace/github/QQhuxuhui/CLIProxyAPIPlus && go test ./internal/api/handlers/management/ -run TestGetKiroUsage -v`
Expected: 所有测试通过

Run: `cd /usr/src/workspace/github/QQhuxuhui/CLIProxyAPIPlus && go test ./internal/api/handlers/management/ -run TestExtractKiro -v`
Expected: 所有测试通过

**Step 3: Commit**

```bash
git add internal/api/handlers/management/kiro_usage_test.go
git commit -m "test(management): add kiro usage endpoint tests"
```

---

## Task 4: 集成测试验证

**Step 1: 完整编译项目**

Run: `cd /usr/src/workspace/github/QQhuxuhui/CLIProxyAPIPlus && go build -o /dev/null ./cmd/cli-proxy-api`
Expected: 编译成功

**Step 2: 运行所有 management 测试**

Run: `cd /usr/src/workspace/github/QQhuxuhui/CLIProxyAPIPlus && go test ./internal/api/handlers/management/... -v`
Expected: 所有测试通过

**Step 3: 最终 Commit（如有遗漏修改）**

```bash
git status
# 如有未提交的修改，进行提交
```

---

## 实现清单总结

| Task | 文件 | 操作 | 说明 |
|------|------|------|------|
| 1 | `internal/api/handlers/management/kiro_usage.go` | 新增 | Kiro 用量接口处理器 |
| 2 | `internal/api/server.go` | 修改 | 注册路由 |
| 3 | `internal/api/handlers/management/kiro_usage_test.go` | 新增 | 单元测试 |
| 4 | - | 验证 | 集成测试 |

## 前端说明

前端（management.html）需要配合更新以调用此接口。由于 management.html 是独立部署的（从 GitHub releases 自动下载），前端改动需要在 `router-for-me/Cli-Proxy-API-Management-Center` 仓库中进行。

前端需要实现：
1. Kiro 卡片的"查看用量"展开功能
2. 调用 `GET /v0/management/kiro-usage?auth_index=X` 接口
3. 展示用量信息（订阅计划、进度条、重置时间）
4. 错误处理和重试机制
