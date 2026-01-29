/**
 * Kiro 配额相关 API
 */

import { apiClient } from './client';
import type { KiroUsageResponse } from '@/types';

export const kiroApi = {
  /**
   * 获取指定 Kiro 账户的配额信息
   * @param authIndex 认证索引
   */
  getUsage: (authIndex: string) =>
    apiClient.get<KiroUsageResponse>(`/kiro-usage?auth_index=${encodeURIComponent(authIndex)}`),
};
