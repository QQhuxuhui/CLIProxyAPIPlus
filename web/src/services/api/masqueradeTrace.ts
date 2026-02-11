/**
 * Masquerade Trace API - 请求头伪装追踪
 */

import { apiClient } from './client';

export interface MasqueradeTraceSummary {
  id: string;
  timestamp: number;
  model: string;
  auth_id: string;
  auth_label: string;
  original_user_id: string;
  masked_user_id: string;
  user_id_changed: boolean;
  headers_modified: number;
}

export interface MasqueradeTraceRecord {
  id: string;
  timestamp: number;
  model: string;
  auth_id: string;
  auth_label: string;
  original_headers: Record<string, string>;
  original_body?: string;
  masked_headers: Record<string, string>;
  masked_body?: string;
  original_user_id: string;
  masked_user_id: string;
  original_session: string;
  masked_session: string;
  hash_source?: string;
}

export interface MasqueradeTraceListResponse {
  traces: MasqueradeTraceSummary[];
  count: number;
  enabled: boolean;
}

export const masqueradeTraceApi = {
  /**
   * 获取 trace 列表（摘要）
   */
  fetchTraces: (): Promise<MasqueradeTraceListResponse> =>
    apiClient.get('/masquerade-trace'),

  /**
   * 获取单条 trace 详情
   */
  fetchTraceById: (id: string): Promise<MasqueradeTraceRecord> =>
    apiClient.get(`/masquerade-trace/${encodeURIComponent(id)}`),

  /**
   * 清除所有 trace 记录
   */
  clearTraces: (): Promise<{ success: boolean; message: string }> =>
    apiClient.delete('/masquerade-trace'),
};
