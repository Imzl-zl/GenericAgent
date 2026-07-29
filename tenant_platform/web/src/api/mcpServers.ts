import { api } from './client';
import type { MCPServer } from './types';

interface MCPServerListResponse {
  servers: MCPServer[];
}

export interface MCPServerWriteInput {
  server_key: string;
  name: string;
  url: string;
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
