import { api } from './client';

export interface AgentRuntimeSettings {
  max_turns: number;
}

export interface IMAggregationSettings {
  window_ms: number;
}

export interface DocumentPoolSettings {
  enabled: boolean;
  max_active: number;
  min_ready: number;
  job_idle_ttl_seconds: number;
  ready_idle_ttl_seconds: number;
  global_queue_limit: number;
  per_tenant_queue_limit: number;
  per_tenant_active_limit: number;
  job_timeout_seconds: number;
  command_timeout_seconds: number;
  version: number;
  updated_by: number;
  updated_at: string;
  reason: string;
  deployment_max_active: number;
  apply_status?: 'applied' | 'pending_retry';
}

export interface DocumentPoolStatus {
  jobs_queued: number;
  jobs_starting: number;
  jobs_running: number;
  instances_creating: number;
  instances_ready: number;
  instances_allocated: number;
  instances_running: number;
  instances_destroying: number;
  instances_lost: number;
  commands_pending: number;
  commands_executing: number;
  oldest_queued_at?: string;
  observed_at: string;
}

export interface UpdateDocumentPoolSettingsInput {
  enabled: boolean;
  max_active: number;
  min_ready: number;
  job_idle_ttl_seconds: number;
  ready_idle_ttl_seconds: number;
  global_queue_limit: number;
  per_tenant_queue_limit: number;
  per_tenant_active_limit: number;
  job_timeout_seconds: number;
  command_timeout_seconds: number;
  expected_version: number;
  reason: string;
}

export async function getIMAggregationSettings(): Promise<IMAggregationSettings> {
  return api.get<IMAggregationSettings>('/v1/admin/settings/im-aggregation');
}

export async function updateIMAggregationSettings(input: IMAggregationSettings): Promise<IMAggregationSettings> {
  return api.put<IMAggregationSettings>('/v1/admin/settings/im-aggregation', input);
}

export async function getAgentRuntimeSettings(): Promise<AgentRuntimeSettings> {
  return api.get<AgentRuntimeSettings>('/v1/admin/settings/agent-runtime');
}

export async function updateAgentRuntimeSettings(input: AgentRuntimeSettings): Promise<AgentRuntimeSettings> {
  return api.put<AgentRuntimeSettings>('/v1/admin/settings/agent-runtime', input);
}

export async function getDocumentPoolSettings(): Promise<DocumentPoolSettings> {
  return api.get<DocumentPoolSettings>('/v1/admin/settings/document-pool');
}

export async function getDocumentPoolStatus(): Promise<DocumentPoolStatus> {
  return api.get<DocumentPoolStatus>('/v1/admin/document-pool/status');
}

export async function updateDocumentPoolSettings(input: UpdateDocumentPoolSettingsInput): Promise<DocumentPoolSettings> {
  return api.put<DocumentPoolSettings>('/v1/admin/settings/document-pool', input);
}
