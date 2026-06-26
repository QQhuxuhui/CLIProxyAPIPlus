# Kiro 高优先级功能详细说明

## 1. Thinking 模式相关

### 核心提交
- `70949929` - fix(kiro): deduplicate thinking stream emission
  - 修复 thinking 流重复发送问题

- `7c9c89da` - fix(kiro): keep thinking enabled across request formats
  - 确保 thinking 在不同请求格式间保持启用

- `d687ee27` - feat(kiro): implement official reasoningContentEvent and improve metadata
  - 实现官方 reasoningContentEvent

- `de0ea3ac` - fix(kiro): Always parse thinking tags from Kiro API responses
  - 始终解析 thinking 标签

- `e889efed` - fix: add signature field to thinking blocks for non-streaming mode
  - 为非流式模式的 thinking 块添加签名字段

### 影响范围
- 流式和非流式响应处理
- thinking 内容的去重和格式化
- 与 Claude Code 的兼容性

---

## 2. 认证修复

### 核心提交
- `27531283` - fix: 添加 Accept 头和重试机制修复 token refresh 401
  - 解决令牌刷新时的 401 错误

- `8f780e72` - fix(kiro): always attempt token refresh on 401 before checking retry count
  - 401 时优先尝试刷新令牌

- `a9ee971e` - fix(kiro): improve auto-refresh and IDC auth file handling
  - 改进自动刷新和 IDC 认证文件处理

- `318f279b` - fix: use clean HTTP client + KiroIDE User-Agent for RefreshSocialToken
  - 使用正确的 User-Agent 刷新令牌

- `98db5aab` - feat: persist refreshed IDC tokens to auth file
  - 持久化刷新后的 IDC tokens

### 影响范围
- 令牌自动刷新机制
- IDC 和社交认证流程
- 401 错误处理逻辑

---

## 3. 消息处理修复

### 核心提交
- `b45ede0b` - fix(kiro): handle empty content in messages to prevent Bad Request errors
  - 处理空 content 防止 Bad Request

- `88872baf` - fix(kiro): handle empty content in Claude format assistant messages
  - 处理 Claude 格式中的空 content

- `4e3bad39` - fix(kiro): handle empty content in current user message for compaction
  - 压缩时处理当前用户消息的空 content

- `086d8d0d` - fix(kiro): prepend placeholder user message when conversation starts with assistant role
  - 对话以 assistant 开始时添加占位符

- `55c3197f` - fix(kiro): merge adjacent assistant messages while preserving tool_calls
  - 合并相邻 assistant 消息同时保留 tool_calls

- `09b19f5c` - fix(kiro): filter orphaned tool_results from compacted conversations
  - 过滤孤立的 tool_results

- `988d8283` - fix(kiro): 在响应端过滤占位符回显，彻底消除空行问题
  - 过滤占位符回显

- `ae463871` - fix(kiro): handle tool_use in content array for compaction requests
  - 处理 content 数组中的 tool_use

### 影响范围
- 消息格式验证
- 对话压缩逻辑
- 工具调用处理
- 空行和占位符问题

---

## 4. 端点切换与动态区域

### 核心提交
- `1e764de0` - feat(kiro): switch to Amazon Q endpoint as primary
  - 主端点切换到 Amazon Q

- `4721c58d` - fix(kiro): correct Amazon Q endpoint URL path
  - 修正 Amazon Q 端点路径

- `38094a23` - feat(kiro): Add dynamic region support for API endpoints
  - 动态区域支持

- `fafef32b` - fix(kiro): Do not use OIDC region for API endpoint
  - 不使用 OIDC 区域作为 API 端点

- `030bf5e6` - feat(kiro): add IDC auth and endpoint improvements, redesign fingerprint system
  - IDC 认证和端点改进，重新设计指纹系统

- `778cf4af` - feat(kiro): add agent-mode and optout headers for non-IDC auth
  - 非 IDC 认证添加 agent-mode 和 optout 头

### 影响范围
- API 请求路由
- 区域选择逻辑
- 请求头设置
- 可能影响现有配置（端点 URL 变化）

---

## 总结与建议

### 关键风险点
1. **端点切换** - 可能需要更新配置文件中的端点 URL
2. **认证流程** - 令牌刷新逻辑变化，需要测试现有认证
3. **消息格式** - 空 content 处理可能影响现有对话流程
4. **Thinking 模式** - 新的事件格式需要客户端支持

### 测试建议
- 完整的认证流程测试（包括令牌刷新）
- 各种消息格式的边界情况测试
- Thinking 模式在流式和非流式下的表现
- 端点切换后的连接稳定性

### 合并顺序建议
1. 先合并消息处理修复（最稳定）
2. 再合并认证修复（核心功能）
3. 然后 Thinking 模式（新功能）
4. 最后端点切换（需要配置调整）

