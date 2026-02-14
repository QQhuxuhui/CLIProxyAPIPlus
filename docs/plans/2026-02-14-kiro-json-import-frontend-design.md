# Kiro JSON 导入前端页面设计

## 背景

后端已实现 `POST /v0/management/kiro/import-json` 接口，支持通过 JSON 数组批量导入 Kiro 账户凭据（Social 和 IdC 两种类型）。现需开发对应的前端页面入口。

## 方案

在 OAuthPage 中，紧跟 Kiro OAuth Card 后面新增一个 "Kiro JSON 导入" Card。状态管理用 `useState`，和页面内 Vertex、iFlow 的模式保持一致。

## API 层

在 `web/src/services/api/kiro.ts` 新增 `importJson` 方法。

请求体：`KiroJsonImportItem[]`

```ts
interface KiroJsonImportItem {
  refreshToken: string;
  provider?: string;      // Google | GitHub | BuilderId | Enterprise
  clientId?: string;
  clientSecret?: string;
  region?: string;
}
```

响应体：

```ts
interface KiroJsonImportResultItem {
  index: number;
  status: 'ok' | 'error';
  email?: string;
  fileName?: string;
  error?: string;
}

interface KiroJsonImportResponse {
  total: number;
  success: number;
  failed: number;
  results: KiroJsonImportResultItem[];
}
```

类型定义放在 `web/src/types/kiro.ts`。

## UI 交互

Card 结构：
- 标题：Kiro 图标 + "Kiro JSON 导入"
- 右上角：导入按钮（带 loading 状态）
- 内容区：
  1. 提示文字：说明 JSON 格式要求
  2. textarea：粘贴 JSON，placeholder 展示示例格式
  3. 文件选择器：选择文件按钮 + 文件名显示（和 Vertex 一致）
  4. 互斥逻辑：上传文件后清空 textarea 并禁用；编辑 textarea 后清除已选文件
  5. 错误提示：status-badge error
  6. 结果列表：每条记录展示 index、状态 badge、email、fileName、error

状态管理：

```ts
interface KiroJsonImportState {
  jsonText: string;
  file?: File;
  fileName: string;
  loading: boolean;
  error?: string;
  result?: KiroJsonImportResponse;
}
```

## 错误处理

- 前端 JSON 解析失败：显示解析错误，不发请求
- 空内容（textarea 为空且无文件）：提示输入内容或选择文件
- 文件类型校验：只接受 `.json`
- 后端 413/400：显示后端错误信息
- 后端 200 部分失败：正常展示结果列表
- 网络错误：通用错误 + notification

## 不做的事情

- JSON 编辑器高亮
- 拖拽上传
- 导入历史记录
- 批量重试失败项

## 改动文件

1. `web/src/types/kiro.ts` — 新增导入相关类型
2. `web/src/services/api/kiro.ts` — 新增 `importJson` 方法
3. `web/src/pages/OAuthPage.tsx` — 新增 Kiro JSON 导入 Card
4. `web/src/pages/OAuthPage.module.scss` — textarea 等样式
5. `web/src/i18n/locales/zh-CN.json` — 中文翻译
6. `web/src/i18n/locales/en.json` — 英文翻译

## i18n

在 `auth_login` 命名空间下新增 `kiro_json_import_*` 系列 key，中英文各 18 条。
