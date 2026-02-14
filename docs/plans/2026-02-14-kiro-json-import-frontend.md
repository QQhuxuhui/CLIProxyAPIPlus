# Kiro JSON 导入前端页面 Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 在 OAuthPage 中新增 Kiro JSON 导入 Card，支持粘贴 JSON 或上传文件批量导入 Kiro 账户凭据。

**Architecture:** 在现有 OAuthPage 内新增一个 Card 组件（和 Vertex JSON 导入、iFlow Cookie 登录同构），用 useState 管理状态。API 层在 kiro.ts 新增 importJson 方法，类型定义在 types/kiro.ts。

**Tech Stack:** React + TypeScript + react-i18next + SCSS Modules + Axios

---

### Task 1: 新增类型定义

**Files:**
- Modify: `web/src/types/kiro.ts` (在文件末尾追加)

**Step 1: 添加导入相关类型**

在 `web/src/types/kiro.ts` 文件末尾追加：

```ts

// ---- JSON 导入相关 ----

export interface KiroJsonImportItem {
  refreshToken: string;
  provider?: string;
  clientId?: string;
  clientSecret?: string;
  region?: string;
}

export interface KiroJsonImportResultItem {
  index: number;
  status: 'ok' | 'error';
  email?: string;
  fileName?: string;
  error?: string;
}

export interface KiroJsonImportResponse {
  total: number;
  success: number;
  failed: number;
  results: KiroJsonImportResultItem[];
}
```

**Step 2: Commit**

```bash
cd web
git add src/types/kiro.ts
git commit -m "feat(kiro): add json import type definitions"
```

---

### Task 2: 新增 API 方法

**Files:**
- Modify: `web/src/services/api/kiro.ts`

**Step 1: 添加 importJson 方法**

将 `web/src/services/api/kiro.ts` 替换为：

```ts
/**
 * Kiro 相关 API
 */

import { apiClient } from './client';
import type { KiroUsageResponse, KiroJsonImportItem, KiroJsonImportResponse } from '@/types';

export const kiroApi = {
  /**
   * 获取指定 Kiro 账户的配额信息
   * @param authIndex 认证索引
   */
  getUsage: (authIndex: string) =>
    apiClient.get<KiroUsageResponse>(`/kiro-usage?auth_index=${encodeURIComponent(authIndex)}`),

  /**
   * 批量导入 Kiro JSON 凭据
   * @param items 账户凭据数组
   */
  importJson: (items: KiroJsonImportItem[]) =>
    apiClient.post<KiroJsonImportResponse>('/kiro/import-json', items),
};
```

**Step 2: Commit**

```bash
git add src/services/api/kiro.ts
git commit -m "feat(kiro): add importJson API method"
```

---

### Task 3: 新增 i18n 翻译

**Files:**
- Modify: `web/src/i18n/locales/zh-CN.json` (在 `kiro_oauth_polling_error` 行之后插入)
- Modify: `web/src/i18n/locales/en.json` (在 `kiro_oauth_polling_error` 行之后插入)

**Step 1: 添加中文翻译**

在 `zh-CN.json` 的 `"kiro_oauth_polling_error": "检查认证状态失败:",` 行之后插入：

```json
    "kiro_json_import_title": "Kiro JSON 导入",
    "kiro_json_import_hint": "粘贴或上传包含 Kiro 账户凭据的 JSON 数组，支持 Social（Google/GitHub）和 IdC（BuilderId/Enterprise）账户。",
    "kiro_json_import_button": "导入",
    "kiro_json_import_textarea_label": "JSON 内容",
    "kiro_json_import_placeholder": "[{\"refreshToken\":\"aor...\",\"provider\":\"Google\"},{\"refreshToken\":\"aor...\",\"clientId\":\"...\",\"clientSecret\":\"...\"}]",
    "kiro_json_import_file_label": "或上传 JSON 文件",
    "kiro_json_import_choose_file": "选择文件",
    "kiro_json_import_file_placeholder": "未选择文件",
    "kiro_json_import_file_required": "请选择 .json 文件",
    "kiro_json_import_empty": "请输入 JSON 内容或选择文件",
    "kiro_json_import_parse_error": "JSON 解析失败",
    "kiro_json_import_result_title": "导入结果",
    "kiro_json_import_result_summary": "共 {{total}} 条，成功 {{success}} 条，失败 {{failed}} 条",
    "kiro_json_import_status_ok": "成功",
    "kiro_json_import_status_error": "失败",
    "kiro_json_import_result_email": "邮箱",
    "kiro_json_import_result_file": "文件",
```

**Step 2: 添加英文翻译**

在 `en.json` 的 `"kiro_oauth_polling_error": "Failed to check authentication status:",` 行之后插入：

```json
    "kiro_json_import_title": "Kiro JSON Import",
    "kiro_json_import_hint": "Paste or upload a JSON array of Kiro account credentials. Supports Social (Google/GitHub) and IdC (BuilderId/Enterprise) accounts.",
    "kiro_json_import_button": "Import",
    "kiro_json_import_textarea_label": "JSON Content",
    "kiro_json_import_placeholder": "[{\"refreshToken\":\"aor...\",\"provider\":\"Google\"},{\"refreshToken\":\"aor...\",\"clientId\":\"...\",\"clientSecret\":\"...\"}]",
    "kiro_json_import_file_label": "Or upload a JSON file",
    "kiro_json_import_choose_file": "Choose File",
    "kiro_json_import_file_placeholder": "No file selected",
    "kiro_json_import_file_required": "Please select a .json file",
    "kiro_json_import_empty": "Please enter JSON content or select a file",
    "kiro_json_import_parse_error": "Failed to parse JSON",
    "kiro_json_import_result_title": "Import Results",
    "kiro_json_import_result_summary": "Total {{total}}, Success {{success}}, Failed {{failed}}",
    "kiro_json_import_status_ok": "Success",
    "kiro_json_import_status_error": "Failed",
    "kiro_json_import_result_email": "Email",
    "kiro_json_import_result_file": "File",
```

**Step 3: Commit**

```bash
git add src/i18n/locales/zh-CN.json src/i18n/locales/en.json
git commit -m "feat(kiro): add json import i18n translations"
```

---

### Task 4: 新增 SCSS 样式

**Files:**
- Modify: `web/src/pages/OAuthPage.module.scss` (在文件末尾追加)

**Step 1: 添加 JSON 导入相关样式**

在 `web/src/pages/OAuthPage.module.scss` 文件末尾追加：

```scss

.jsonImportTextarea {
  width: 100%;
  min-height: 120px;
  padding: 10px 12px;
  border: 1px solid var(--border-color);
  border-radius: $radius-md;
  background: var(--bg-primary);
  color: var(--text-primary);
  font-family: 'SF Mono', 'Fira Code', 'Fira Mono', Menlo, Consolas, monospace;
  font-size: 13px;
  line-height: 1.5;
  resize: vertical;

  &::placeholder {
    color: var(--text-secondary);
    font-size: 12px;
  }

  &:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }
}

.jsonImportFileRow {
  display: flex;
  align-items: center;
  gap: $spacing-sm;
  margin-top: $spacing-sm;
}

.importResultSummary {
  font-size: 14px;
  font-weight: 600;
  color: var(--text-primary);
  margin-bottom: $spacing-sm;
}

.importResultList {
  display: flex;
  flex-direction: column;
  gap: $spacing-xs;
}

.importResultItem {
  display: flex;
  align-items: baseline;
  gap: $spacing-sm;
  font-size: 13px;
  line-height: 1.5;
  padding: $spacing-xs 0;
  border-bottom: 1px solid var(--border-color);

  &:last-child {
    border-bottom: none;
  }
}

.importResultIndex {
  color: var(--text-secondary);
  min-width: 32px;
}

.importResultDetail {
  flex: 1;
  color: var(--text-primary);
  word-break: break-all;
}
```

**Step 2: Commit**

```bash
git add src/pages/OAuthPage.module.scss
git commit -m "feat(kiro): add json import styles"
```

---

### Task 5: 在 OAuthPage 中新增 Kiro JSON 导入 Card

**Files:**
- Modify: `web/src/pages/OAuthPage.tsx`

这是最核心的改动。需要在 OAuthPage 中：

1. 新增 import 语句
2. 新增状态和处理函数
3. 在 Kiro OAuth Card 后面插入新 Card

**Step 1: 新增 import**

在 `OAuthPage.tsx` 顶部的 import 区域：

- 在 `import { oauthApi, ... } from '@/services/api/oauth';` 之后添加：
```ts
import { kiroApi } from '@/services/api/kiro';
import type { KiroJsonImportResponse } from '@/types';
```

- 在 `import { vertexApi, ... } from '@/services/api/vertex';` 之后（已有的 import 行），确保 `ChangeEvent` 已在 react import 中（已有）。

**Step 2: 新增状态定义**

在 `OAuthPage` 函数内部，在 `const vertexFileInputRef = useRef<HTMLInputElement | null>(null);` 之后添加：

```ts
  // Kiro JSON Import
  const [kiroJsonImport, setKiroJsonImport] = useState<{
    jsonText: string;
    file?: File;
    fileName: string;
    loading: boolean;
    error?: string;
    result?: KiroJsonImportResponse;
  }>({ jsonText: '', fileName: '', loading: false });
  const kiroJsonFileInputRef = useRef<HTMLInputElement | null>(null);
```

**Step 3: 新增处理函数**

在 `handleVertexImport` 函数之后添加：

```ts
  const handleKiroJsonTextChange = (e: ChangeEvent<HTMLTextAreaElement>) => {
    setKiroJsonImport((prev) => ({
      ...prev,
      jsonText: e.target.value,
      file: undefined,
      fileName: '',
      error: undefined,
      result: undefined,
    }));
  };

  const handleKiroJsonFilePick = () => {
    kiroJsonFileInputRef.current?.click();
  };

  const handleKiroJsonFileChange = (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    if (!file) return;
    if (!file.name.endsWith('.json')) {
      showNotification(t('auth_login.kiro_json_import_file_required'), 'warning');
      event.target.value = '';
      return;
    }
    setKiroJsonImport((prev) => ({
      ...prev,
      file,
      fileName: file.name,
      jsonText: '',
      error: undefined,
      result: undefined,
    }));
    event.target.value = '';
  };

  const handleKiroJsonImport = async () => {
    let jsonText = kiroJsonImport.jsonText.trim();

    if (!jsonText && kiroJsonImport.file) {
      try {
        jsonText = await kiroJsonImport.file.text();
      } catch {
        setKiroJsonImport((prev) => ({ ...prev, error: t('auth_login.kiro_json_import_parse_error') }));
        return;
      }
    }

    if (!jsonText) {
      const message = t('auth_login.kiro_json_import_empty');
      setKiroJsonImport((prev) => ({ ...prev, error: message }));
      showNotification(message, 'warning');
      return;
    }

    let items: unknown;
    try {
      items = JSON.parse(jsonText);
    } catch {
      const message = t('auth_login.kiro_json_import_parse_error');
      setKiroJsonImport((prev) => ({ ...prev, error: message }));
      showNotification(message, 'error');
      return;
    }

    if (!Array.isArray(items) || items.length === 0) {
      const message = t('auth_login.kiro_json_import_parse_error');
      setKiroJsonImport((prev) => ({ ...prev, error: message }));
      showNotification(message, 'error');
      return;
    }

    setKiroJsonImport((prev) => ({ ...prev, loading: true, error: undefined, result: undefined }));
    try {
      const res = await kiroApi.importJson(items);
      setKiroJsonImport((prev) => ({ ...prev, loading: false, result: res }));
      if (res.failed === 0) {
        showNotification(
          t('auth_login.kiro_json_import_result_summary', { total: res.total, success: res.success, failed: res.failed }),
          'success'
        );
      } else {
        showNotification(
          t('auth_login.kiro_json_import_result_summary', { total: res.total, success: res.success, failed: res.failed }),
          'warning'
        );
      }
    } catch (err: any) {
      const message = err?.message || '';
      setKiroJsonImport((prev) => ({ ...prev, loading: false, error: message }));
      showNotification(message || t('auth_login.kiro_json_import_parse_error'), 'error');
    }
  };
```

**Step 4: 在 JSX 中插入 Card**

在 OAuthPage 的 return JSX 中，找到 `{PROVIDERS.map((provider) => { ... })}` 闭合的 `})}` 之后、`{/* Vertex JSON 登录 */}` 之前，插入：

```tsx
        {/* Kiro JSON 导入 */}
        <Card
          title={
            <span className={styles.cardTitle}>
              <img src={iconKiro} alt="" className={styles.cardTitleIcon} />
              {t('auth_login.kiro_json_import_title')}
            </span>
          }
          extra={
            <Button onClick={handleKiroJsonImport} loading={kiroJsonImport.loading}>
              {t('auth_login.kiro_json_import_button')}
            </Button>
          }
        >
          <div className="hint">{t('auth_login.kiro_json_import_hint')}</div>
          <div className="form-group" style={{ marginTop: 12 }}>
            <label>{t('auth_login.kiro_json_import_textarea_label')}</label>
            <textarea
              className={styles.jsonImportTextarea}
              value={kiroJsonImport.jsonText}
              onChange={handleKiroJsonTextChange}
              placeholder={t('auth_login.kiro_json_import_placeholder')}
              disabled={Boolean(kiroJsonImport.file)}
            />
          </div>
          <div className="form-group">
            <label>{t('auth_login.kiro_json_import_file_label')}</label>
            <div className={styles.jsonImportFileRow}>
              <Button variant="secondary" size="sm" onClick={handleKiroJsonFilePick}>
                {t('auth_login.kiro_json_import_choose_file')}
              </Button>
              <div
                className={`${styles.fileName} ${
                  kiroJsonImport.fileName ? '' : styles.fileNamePlaceholder
                }`.trim()}
              >
                {kiroJsonImport.fileName || t('auth_login.kiro_json_import_file_placeholder')}
              </div>
            </div>
            <input
              ref={kiroJsonFileInputRef}
              type="file"
              accept=".json,application/json"
              style={{ display: 'none' }}
              onChange={handleKiroJsonFileChange}
            />
          </div>
          {kiroJsonImport.error && (
            <div className="status-badge error" style={{ marginTop: 8 }}>
              {kiroJsonImport.error}
            </div>
          )}
          {kiroJsonImport.result && (
            <div className="connection-box" style={{ marginTop: 12 }}>
              <div className={styles.importResultSummary}>
                {t('auth_login.kiro_json_import_result_summary', {
                  total: kiroJsonImport.result.total,
                  success: kiroJsonImport.result.success,
                  failed: kiroJsonImport.result.failed,
                })}
              </div>
              <div className={styles.importResultList}>
                {kiroJsonImport.result.results.map((item) => (
                  <div key={item.index} className={styles.importResultItem}>
                    <span className={styles.importResultIndex}>#{item.index}</span>
                    <span
                      className={`status-badge ${item.status === 'ok' ? 'success' : 'error'}`}
                      style={{ fontSize: 12, padding: '2px 8px' }}
                    >
                      {item.status === 'ok'
                        ? t('auth_login.kiro_json_import_status_ok')
                        : t('auth_login.kiro_json_import_status_error')}
                    </span>
                    <span className={styles.importResultDetail}>
                      {item.status === 'ok' ? (
                        <>
                          {item.email && (
                            <span>{t('auth_login.kiro_json_import_result_email')}: {item.email}</span>
                          )}
                          {item.email && item.fileName && <span> · </span>}
                          {item.fileName && (
                            <span>{t('auth_login.kiro_json_import_result_file')}: {item.fileName}</span>
                          )}
                        </>
                      ) : (
                        <span>{item.error}</span>
                      )}
                    </span>
                  </div>
                ))}
              </div>
            </div>
          )}
        </Card>
```

**Step 5: Commit**

```bash
git add src/pages/OAuthPage.tsx
git commit -m "feat(kiro): add json import card to OAuthPage"
```

---

### Task 6: 验证构建

**Step 1: 运行构建检查**

```bash
cd /root/workspace/web
npx tsc --noEmit
```

Expected: 无类型错误

**Step 2: 运行 lint**

```bash
npx eslint src/pages/OAuthPage.tsx src/services/api/kiro.ts src/types/kiro.ts --no-error-on-unmatched-pattern
```

Expected: 无 lint 错误

**Step 3: 最终 commit（如有修复）**

```bash
git add -A
git commit -m "fix(kiro): address lint/type issues in json import"
```
