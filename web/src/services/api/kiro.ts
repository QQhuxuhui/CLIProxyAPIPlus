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
