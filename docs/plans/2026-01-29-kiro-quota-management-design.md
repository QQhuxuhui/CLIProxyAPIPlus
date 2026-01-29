# Kiro Quota Management Feature Design

## Overview

Add Kiro quota/usage management to the AI Providers page, allowing users to view quota information and delete authorized Kiro accounts.

## Requirements

- Display Kiro accounts in AI Providers page alongside other providers
- Show detailed quota information: email, subscription, usage (current/limit/percentage), reset time
- Support deleting Kiro accounts
- Reuse existing auth-files API for account list, call kiro-usage API for quota details

## Component Structure

```
web/src/components/providers/KiroSection/
├── KiroSection.tsx      # Main component displaying Kiro account list (uses ProviderList renderContent)
└── index.ts             # Exports
```

Note: Following the pattern of other provider sections (ClaudeSection, CodexSection), quota display is implemented directly in KiroSection using `ProviderList`'s `renderContent` callback, rather than a separate card component.

## API Layer

### New File: `web/src/services/api/kiro.ts`

```typescript
export interface KiroUsageResponse {
  success: boolean;
  data?: {
    email: string;
    subscription: string;
    usage: {
      current: number;
      limit: number;
      percentage: number;
    };
    reset: {
      days_until: number;
      next_date: string;
    };
  };
  error?: string;
}

export const kiroApi = {
  getUsage: (authIndex: string) =>
    apiClient.get<KiroUsageResponse>(`/kiro-usage?auth_index=${encodeURIComponent(authIndex)}`),
};
```

### New File: `web/src/types/kiro.ts`

```typescript
export interface KiroAccount {
  name: string;           // Auth file name for deletion
  authIndex: string;      // For calling kiro-usage API
  email?: string;         // From usage API
  subscription?: string;
  usage?: {
    current: number;
    limit: number;
    percentage: number;
  };
  reset?: {
    daysUntil: number;
    nextDate: string;
  };
  loading?: boolean;      // Quota loading state
  error?: string;         // Quota loading error
}
```

## UI Design

### Account Item Layout (renderContent)

```
┌─────────────────────────────────────────────────────┐
│ [Kiro Icon] user@example.com             [Delete]   │
├─────────────────────────────────────────────────────┤
│ Subscription: Pro Plan                              │
│                                                     │
│ Usage: 150 / 500 (30%)                             │
│ [████████░░░░░░░░░░░░░░░░░░░░] 30%                 │
│                                                     │
│ Resets in: 5 days (2026-02-03)                     │
└─────────────────────────────────────────────────────┘
```

### State Handling

- **Loading**: Show skeleton or loading spinner
- **Error**: Show error message with retry button
- **Quota warning (>80%)**: Progress bar in orange
- **Quota full (100%)**: Progress bar in red

### KiroSection Structure

- Header: Kiro icon + "Kiro" title + "Go to Authorize" button (navigates to OAuth page)
- Empty state: Prompt user to go to OAuth page to add Kiro accounts
- List: Reuse existing `ProviderList` component for consistent styling

## Data Flow

1. `AiProvidersPage` loads and calls `authFilesApi.list()` to get all auth files
2. Filter entries where `provider === 'kiro'` as Kiro account list
3. For each Kiro account, use `authIndex` to call `kiroApi.getUsage(authIndex)` for quota info
4. Delete operation calls `authFilesApi.deleteFile(name)`

## i18n Keys

### English (en.json)

```json
{
  "ai_providers": {
    "kiro_title": "Kiro",
    "kiro_empty_title": "No Kiro accounts",
    "kiro_empty_desc": "Go to OAuth page to authorize Kiro accounts",
    "kiro_go_auth": "Go to Authorize",
    "kiro_subscription": "Subscription",
    "kiro_usage": "Usage",
    "kiro_reset": "Resets in",
    "kiro_reset_date": "Reset date",
    "kiro_days": "days",
    "kiro_delete_title": "Delete Kiro Account",
    "kiro_delete_confirm": "Are you sure you want to delete this Kiro account?",
    "kiro_usage_loading": "Loading usage...",
    "kiro_usage_error": "Failed to load usage",
    "kiro_retry": "Retry"
  },
  "notification": {
    "kiro_deleted": "Kiro account deleted"
  }
}
```

### Chinese (zh-CN.json)

```json
{
  "ai_providers": {
    "kiro_title": "Kiro",
    "kiro_empty_title": "暂无 Kiro 账户",
    "kiro_empty_desc": "前往 OAuth 页面授权 Kiro 账户",
    "kiro_go_auth": "前往授权",
    "kiro_subscription": "订阅类型",
    "kiro_usage": "用量",
    "kiro_reset": "重置时间",
    "kiro_reset_date": "重置日期",
    "kiro_days": "天",
    "kiro_delete_title": "删除 Kiro 账户",
    "kiro_delete_confirm": "确定要删除此 Kiro 账户吗？",
    "kiro_usage_loading": "加载用量中...",
    "kiro_usage_error": "加载用量失败",
    "kiro_retry": "重试"
  },
  "notification": {
    "kiro_deleted": "Kiro 账户已删除"
  }
}
```

## Page Integration

In `AiProvidersPage.tsx`:

1. Add `authFilesApi.list()` call in `loadConfigs`
2. Add `kiroAccounts` state to store filtered Kiro accounts
3. Add `<KiroSection />` component between Claude and Vertex sections

## Implementation Tasks

1. Create `web/src/types/kiro.ts` - Type definitions
2. Create `web/src/services/api/kiro.ts` - API service
3. Create `web/src/components/providers/KiroSection/KiroSection.tsx` - Main component
4. Update `web/src/components/providers/index.ts` - Export KiroSection
5. Update `web/src/pages/AiProvidersPage.tsx` - Integrate KiroSection
6. Update `web/src/i18n/locales/en.json` - English translations
7. Update `web/src/i18n/locales/zh-CN.json` - Chinese translations
