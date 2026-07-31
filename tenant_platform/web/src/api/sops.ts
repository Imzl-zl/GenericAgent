import { api, userApi } from './client';

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

export interface SOPCandidate {
  candidate_id: string;
  remote_sop_id: string;
  title: string;
  description: string;
  file_type: string;
  content: string;
  source_digest: string;
  status: 'pending' | 'approved' | 'rejected';
  review_note: string;
  reviewed_at?: string;
}

export interface SOPRegistryItem {
  version_id: string;
  entry_id: string;
  candidate_id: string;
  version: number;
  title: string;
  description: string;
  content: string;
  digest: string;
  approved_at: string;
  loaded: boolean;
}

export interface LoadedSOP {
  title: string;
  description: string;
  content: string;
  digest: string;
  version: number;
}

export const getSophubBinding = () => api.get<SophubBindingStatus>('/v1/admin/sophub/binding');

export const bindSophub = (apiKey: string) =>
  api.put<SophubBindingStatus>('/v1/admin/sophub/binding', { api_key: apiKey });

export const searchSophub = (query: string, page = 1, pageSize = 24) => {
  const params = new URLSearchParams({ q: query, page: String(page), page_size: String(pageSize) });
  return api.get<SophubSearchResult>(`/v1/admin/sophub/search?${params.toString()}`);
};

export const importSOPCandidate = (remoteSOPId: string) =>
  api.post<SOPCandidate>('/v1/admin/sophub/candidates/import', { remote_sop_id: remoteSOPId });

export const listSOPCandidates = () =>
  api.get<{ candidates: SOPCandidate[] }>('/v1/admin/sop-candidates').then((reply) => reply.candidates);

export const approveSOPCandidate = (candidateId: string) =>
  api.post<SOPRegistryItem>(`/v1/admin/sop-candidates/${candidateId}/approve`);

export const rejectSOPCandidate = (candidateId: string, note: string) =>
  api.post<{ rejected: boolean }>(`/v1/admin/sop-candidates/${candidateId}/reject`, { note });

export const listSOPRegistry = () =>
  api.get<{ sops: SOPRegistryItem[] }>('/v1/admin/sops').then((reply) => reply.sops);

export const loadSOPVersion = (versionId: string) =>
  api.post(`/v1/admin/sop-versions/${versionId}/load`);

export const unloadSOPEntry = (entryId: string) =>
  api.post(`/v1/admin/sops/${entryId}/unload`);

export const listLoadedSOPs = () =>
  userApi.get<{ sops: LoadedSOP[] }>('/v1/sops').then((reply) => reply.sops);
