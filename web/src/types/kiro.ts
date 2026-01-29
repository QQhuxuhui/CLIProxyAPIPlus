/**
 * Kiro 账户相关类型定义
 */

export interface KiroUsage {
  current: number;
  limit: number;
  percentage: number;
}

export interface KiroReset {
  daysUntil: number;
  nextDate: string;
}

export interface KiroAccount {
  name: string;           // 认证文件名，用于删除
  authIndex: string;      // 用于调用 kiro-usage API
  email?: string;         // 从 usage API 获取
  subscription?: string;
  usage?: KiroUsage;
  reset?: KiroReset;
  loading?: boolean;      // 配额加载状态
  error?: string;         // 配额加载错误
  deleting?: boolean;     // 删除操作进行中
}

export interface KiroUsageResponseData {
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
}

export interface KiroUsageResponse {
  success: boolean;
  data?: KiroUsageResponseData;
  error?: string;
}
