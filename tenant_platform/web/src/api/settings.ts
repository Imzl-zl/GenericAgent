import { api } from './client';

export interface AgentRuntimeSettings {
  max_turns: number;
}

export interface IMAggregationSettings {
  window_ms: number;
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
