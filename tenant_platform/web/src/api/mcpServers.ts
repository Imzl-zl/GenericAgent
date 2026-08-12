import { api } from './client';
import type { MCPQuotaLimit, MCPServer } from './types';

interface MCPServerListResponse {
  servers: MCPServer[];
}

export interface MCPServerWriteInput {
  server_key: string;
  name: string;
  transport?: 'http' | 'stdio';
  url?: string;
  /** 平台侧持有的凭据头; 更新时掩码值(*** 结尾)保留原 key */
  headers?: Record<string, string>;
  command?: string;
  args?: string[];
  timeout_seconds: number;
}

export async function listMCPServers(): Promise<MCPServer[]> {
  const response = await api.get<MCPServerListResponse>('/v1/admin/mcp-servers');
  return response.servers;
}

export async function createMCPServer(input: MCPServerWriteInput): Promise<MCPServer> {
  return api.post<MCPServer>('/v1/admin/mcp-servers', input);
}

export async function updateMCPServer(id: number, input: MCPServerWriteInput): Promise<MCPServer> {
  return api.put<MCPServer>(`/v1/admin/mcp-servers/${id}`, input);
}

export async function deleteMCPServer(id: number): Promise<void> {
  return api.delete<void>(`/v1/admin/mcp-servers/${id}`);
}

export async function setMCPServerEnabled(id: number, enabled: boolean): Promise<MCPServer> {
  return api.post<MCPServer>(`/v1/admin/mcp-servers/${id}/${enabled ? 'enable' : 'disable'}`);
}

export interface MCPQuotaListResponse {
  quotas: MCPQuotaLimit[];
}

export async function listMCPQuotas(ownerKey?: string): Promise<MCPQuotaLimit[]> {
  const query = ownerKey ? `?owner_key=${encodeURIComponent(ownerKey)}` : '';
  const response = await api.get<MCPQuotaListResponse>(`/v1/admin/mcp-quotas${query}`);
  return response.quotas;
}

export async function upsertMCPQuota(limit: MCPQuotaLimit): Promise<MCPQuotaLimit> {
  return api.put<MCPQuotaLimit>('/v1/admin/mcp-quotas', limit);
}

export async function deleteMCPQuota(ownerKey: string, serverId: string, period: string): Promise<void> {
  const query = `owner_key=${encodeURIComponent(ownerKey)}&server_id=${encodeURIComponent(serverId)}&period=${encodeURIComponent(period)}`;
  await api.delete<void>(`/v1/admin/mcp-quotas?${query}`);
}
