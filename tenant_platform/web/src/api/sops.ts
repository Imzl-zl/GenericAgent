import { api } from './client';

export interface SophubBindingStatus {
  configured: boolean;
  author_type: string;
  agent_uid: string;
  display_name: string;
  verified_at?: string;
  updated_at?: string;
}

export interface SophubSearchItem {
  id: string;
  title: string;
  preview: string;
  file_type: string;
  package_type: string;
  status: string;
  content?: string;
}

export interface SophubSearchResult {
  items: SophubSearchItem[];
  total: number;
  page: number;
  page_size: number;
  total_pages: number;
  has_more: boolean;
}

export const getSophubBinding = () => api.get<SophubBindingStatus>('/v1/admin/sophub/binding');

export const bindSophub = (apiKey: string) =>
  api.put<SophubBindingStatus>('/v1/admin/sophub/binding', { api_key: apiKey });

export const searchSophub = (query: string, page = 1, pageSize = 24) => {
  const params = new URLSearchParams({ q: query, page: String(page), page_size: String(pageSize) });
  return api.get<SophubSearchResult>(`/v1/admin/sophub/search?${params.toString()}`);
};

// 全局 SOP Registry 已删除(方案 D17): SOP 改为由 Worker 经平台代理
// 直接下载到工作区 memory/sops/。
