# Kiro 功能更新总结

> 统计时间：2025年12月6日 至今
> 总计：154 个相关提交

## 一、核心功能增强 (Features)

### 1. 认证与授权
- ✅ IDC 认证和端点改进，重新设计指纹系统
- ✅ AWS Builder ID 授权码流程认证增强
- ✅ OAuth 模型名称映射支持
- ✅ 后台令牌自动刷新通知机制
- ✅ 手动令牌刷新按钮（Web UI）
- ✅ 持久化刷新后的 IDC tokens
- ✅ JSON 凭证导入功能（含验证）
- ✅ 动态区域支持（API 端点）

### 2. 模型支持
- ✅ Claude Opus 4.6 支持
- ✅ Sonnet 4.6 模型别名
- ✅ 新增模型：deepseek-3.2, minimax-m2.1, qwen3-coder-next, gpt-4o, gpt-4, gpt-4-turbo, gpt-3.5-turbo
- ✅ 默认 Kiro 模型别名（标准 Claude 模型名）
- ✅ Kiro channel 支持

### 3. 缓存系统
- ✅ 模拟 cache token 数据（Claude 风格缓存命中）
- ✅ 模拟 cache_creation_input_tokens
- ✅ 缓存命中率随机波动（60%-85%，大上下文最高 90%）
- ✅ 缓存模拟参数可配置化（config.yaml 热重载）
- ✅ 缓存比例提高，匹配 Claude Code 长对话场景

### 4. Thinking 模式
- ✅ 多端点回退 & thinking 模式支持
- ✅ 增强 thinking 支持和截断问题修复
- ✅ 实现官方 reasoningContentEvent
- ✅ 跨请求格式保持 thinking 启用
- ✅ thinking 流去重
- ✅ 始终解析 thinking 标签
- ✅ Token 使用交叉验证和 thinking 模式处理简化

### 5. 工具调用与 MCP
- ✅ 动态工具压缩功能
- ✅ Web Search MCP 集成（流式和非流式）
- ✅ 处理 tool_use 在 content 数组中
- ✅ 过滤孤立的 tool_results
- ✅ OpenAI 格式正确处理 tool results

### 6. 端点与网络
- ✅ 切换到 Amazon Q 端点作为主端点
- ✅ 修正 Amazon Q 端点 URL 路径
- ✅ 非 IDC 认证添加 agent-mode 和 optout 头
- ✅ 不使用 OIDC 区域作为 API 端点

### 7. 用户界面
- ✅ Web UI 添加 Kiro OAuth 登录入口
- ✅ JSON 导入功能（卡片、样式、类型定义、i18n）
- ✅ 配额管理页面添加 Kiro 配额
- ✅ 前端认证入口

### 8. 其他功能
- ✅ 令牌额度查询 API 兼容
- ✅ 使用端点和管理 API
- ✅ 代码优化重构 + OpenAI 翻译器实现
- ✅ 增强请求格式、流处理和使用跟踪
- ✅ 重试、去重和事件过滤
- ✅ contextUsageEvent 处理器

## 二、Bug 修复 (Fixes)

### 1. 认证相关
- 🔧 令牌刷新失败（401 错误）
- 🔧 添加 Accept 头和重试机制修复 token refresh 401
- 🔧 401 时始终尝试令牌刷新（检查重试次数前）
- 🔧 改进自动刷新和 IDC 认证文件处理
- 🔧 从 JWT 提取邮箱用于唯一令牌导入文件名
- 🔧 协议处理器中的额外引号

### 2. 消息处理
- 🔧 处理空 content 防止 Bad Request 错误
- 🔧 处理 Claude 格式 assistant 消息中的空 content
- 🔧 处理当前用户消息空 content（压缩）
- 🔧 对话以 assistant 角色开始时添加占位符用户消息
- 🔧 合并相邻 assistant 消息同时保留 tool_calls
- 🔧 过滤占位符回显，彻底消除空行问题
- 🔧 修剪占位符回显并对齐文件名测试

### 3. 缓存与 Token
- 🔧 input_tokens 应只包含未缓存部分（符合 Claude API 规范）
- 🔧 分离缓存读取/创建 tokens 并收紧模拟
- 🔧 contextUsagePercentage 覆盖 InputTokens 时减去已有缓存数据
- 🔧 传播缓存 token 计数到下游使用输出
- 🔧 补全 Claude API 响应格式缺失字段
- 🔧 处理 kiro 非流式请求的零 output_tokens

### 4. 图片与格式
- 🔧 修复 base64 图片格式转换问题
- 🔧 修复错误的 application/cbor 请求处理逻辑
- 🔧 翻译器格式不匹配（OpenAI 协议）
- 🔧 添加 SSE event: 前缀（Claude 客户端兼容性）

### 5. 模型与配置
- 🔧 修复 kiro 模型列表缺失
- 🔧 移除不稳定的 kiro-auto 模型
- 🔧 重新添加 kiro-auto 到注册表
- 🔧 移除冗余的 kiro 模型定义条目
- 🔧 保留显式删除的 kiro 别名（跨配置重载）
- 🔧 规范化 kiro 重置时间戳

### 6. 工具与截断
- 🔧 处理 Write 工具截断（内容超过 API 限制）
- 🔧 跳过 _partial 字段（可能包含幻觉路径）
- 🔧 支持 OR-group 字段匹配（截断检测器）
- 🔧 按需大小缩放描述压缩

### 7. UI 与体验
- 🔧 改进 JSON 导入 UX 和 lint 合规性
- 🔧 GitHub Copilot executor 上游调用前剥离模型后缀

## 三、重构 (Refactoring)

- 🔄 代码优化重构 + OpenAI 翻译器实现
- 🔄 使用 CodeWhisperer 端点，改进工具调用支持
- 🔄 移除 OpenAI 翻译器中未使用变量
- 🔄 提取默认 assistant 内容到共享常量
- 🔄 Web Search 对齐到 executor 层模式
- 🔄 使用 payloadRequestedModel 作为响应模型名

## 四、建议合并优先级

### 高优先级（核心功能和稳定性）
1. **Thinking 模式相关** - 官方 reasoningContentEvent、去重、签名字段等
2. **认证修复** - 令牌刷新 401、IDC 认证改进
3. **消息处理修复** - 空 content、空行、tool_results 等
4. **端点切换** - Amazon Q 端点、动态区域支持

### 中优先级（功能增强）
5. **新模型支持** - Claude Opus 4.6、Sonnet 4.6 等
6. **缓存系统** - 模拟 cache token、可配置化
7. **工具压缩** - 动态工具压缩功能
8. **Web Search MCP 集成**

### 低优先级（UI 和便利性）
9. **JSON 导入功能** - 凭证导入 UI
10. **配额管理** - Kiro 配额页面
11. **默认模型别名**

## 五、测试相关

- ✅ 补充缓存 token 应用逻辑的单元测试
- ✅ 添加 kiro usage 端点测试
- ✅ 添加 [CACHE-DIAG] 日志追踪缓存 token 数据

## 六、文档更新

- 📝 Kiro OAuth web 认证端点文档 (/v0/oauth/kiro)
- 📝 JSON 导入前端实现计划
- 📝 JSON 导入前端设计
- 📝 github-copilot 和 kiro 添加到 oauth-excluded-models 文档

## 七、注意事项

### 兼容性风险
- 端点切换到 Amazon Q 可能影响现有配置
- 缓存模拟逻辑变化可能影响 token 计费统计
- 消息格式处理变化需要测试现有对话流程

### 建议测试重点
1. 认证流程（特别是 token 刷新）
2. Thinking 模式在不同场景下的表现
3. 缓存 token 统计准确性
4. 工具调用和 MCP 集成
5. 多模型切换稳定性

### 合并建议
建议分批次合并，每批次后进行充分测试：
- 第一批：核心稳定性修复（认证、消息处理）
- 第二批：功能增强（新模型、缓存系统）
- 第三批：UI 和便利性功能

