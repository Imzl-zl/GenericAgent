import { api } from './client';

export interface AgentRuntimeSettings {
  max_turns: number;
}

export interface IMAggregationSettings {
  window_ms: number;
}

export type IMStreamingMode = 'off' | 'final_only' | 'streaming';

export interface IMStreamingSettings {
  mode: IMStreamingMode;
}

export const IM_STREAMING_MODE_LABELS: Record<IMStreamingMode, string> = {
  off: '完全关闭（只发最终结果）',
  final_only: '只发最终结果',
  streaming: '流式输出（私聊打字机）',
};

export const IM_STREAMING_MODE_OPTIONS: IMStreamingMode[] = ['streaming', 'final_only', 'off'];

export async function getIMStreamingSettings(): Promise<IMStreamingSettings> {
  return api.get<IMStreamingSettings>('/v1/admin/settings/im-streaming');
}

export async function updateIMStreamingSettings(input: IMStreamingSettings): Promise<IMStreamingSettings> {
  return api.put<IMStreamingSettings>('/v1/admin/settings/im-streaming', input);
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
