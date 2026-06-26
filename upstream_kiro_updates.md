# 上游 router-for-me 的 Kiro 更新

> 自你最后一次合并 (acb1066d) 后，上游只有 **4 个** kiro 相关更新

## 更新列表

### 1. c5185168 - 移除工具压缩和修复 Bug
**作者**: CheesesNguyen
**日期**: 2026-03-05

**主要变更**:
- ❌ 移除 SOFT_LIMIT_REACHED 标记注入逻辑
- ❌ 移除 SOFT_LIMIT_REACHED 检测逻辑
- ❌ 移除 tool_compression.go 及相关常量
- 🔧 修复 truncation_detector: string(rune(len)) 产生 Unicode 字符而非十进制字符串
- 🔧 修复 WebSearchToolUseId 被非 web-search 工具覆盖
- 🔧 修复 model_definitions.go 中重复的 kiro 条目

**影响文件**: 9 个文件，删除 323 行代码

---

### 2. 82df5bf8 - Merge PR #395 from Xm798/feat/kiro
**作者**: Luis Pater
**日期**: 合并提交

这是一个合并提交，包含了下面两个功能更新。

---

### 3. 9032042c - 添加 Sonnet 4.6 模型别名
**作者**: Cyrus
**日期**: 2026-02-27

**主要变更**:
- ✅ 添加 Claude Sonnet 4.6 模型别名支持

**影响**: 模型注册表更新

---

### 4. 030bf5e6 - IDC 认证和端点改进，重新设计指纹系统
**作者**: Cyrus
**日期**: 2026-02-27

**主要变更**:
- ✅ 添加 IAM Identity Center (IDC) 认证
  - CLI 参数: --kiro-idc-login, --kiro-idc-start-url, --kiro-idc-region
  - IDC 登录流程
- ✅ Execute/ExecuteStream 中自动获取 ProfileArn（导入的 IDC 账户）
- ✅ 简化端点偏好设置（基于 map 的别名查找）
- ✅ 重新设计指纹系统为全局单例，支持外部配置和按账户确定性生成
- ✅ 添加 StartURL 和 FingerprintConfig 字段到 Kiro 配置
- ✅ 添加 AgentContinuationID/AgentTaskType 支持
- ✅ 添加全面的测试（executor, fingerprint, SSO OIDC, AWS helpers）
- 📝 添加 CLI 登录文档到 README

**影响文件**: 15+ 个文件，大量代码重构和测试增强

---

## 总结

上游实际只有 **4 个提交**，主要集中在：

1. **移除功能** - 移除工具压缩和 SOFT_LIMIT_REACHED 逻辑
2. **新模型** - Sonnet 4.6 支持
3. **IDC 认证** - 完整的 IAM Identity Center 认证系统

## 合并建议

### 高优先级
- **030bf5e6** - IDC 认证系统（如果你需要 IDC 支持）
- **9032042c** - Sonnet 4.6 模型别名

### 需要评估
- **c5185168** - 移除工具压缩逻辑
  - ⚠️ 这会删除你可能正在使用的功能
  - 建议先检查你的代码是否依赖 tool_compression.go
  - 检查是否使用了 SOFT_LIMIT_REACHED 相关逻辑

## 注意事项

之前我分析的 154 个提交，大部分是**你本地的提交**，不是上游的更新。抱歉造成混淆！
