import { api } from './client';
import type {
  GASessionConfig,
  LLMProvider,
  LLMProviderType,
  ProviderTransportConfig,
} from './types';

interface ProviderListResponse {
  providers: LLMProvider[];
}

interface ProviderWriteFields {
  name: string;
  provider_type: LLMProviderType;
  base_url: string;
  model: string;
  session_config: GASessionConfig;
  transport_config: ProviderTransportConfig;
}

export interface CreateProviderInput extends ProviderWriteFields {
  api_key: string;
}

export interface UpdateProviderInput extends ProviderWriteFields {
  api_key?: string;
}

export async function listProviders(): Promise<LLMProvider[]> {
  const response = await api.get<ProviderListResponse>('/v1/admin/llm-providers');
  return response.providers;
}

export async function createProvider(input: CreateProviderInput): Promise<LLMProvider> {
  return api.post<LLMProvider>('/v1/admin/llm-providers', input);
}

export async function updateProvider(
  providerId: number,
  input: UpdateProviderInput,
): Promise<LLMProvider> {
  return api.put<LLMProvider>(`/v1/admin/llm-providers/${providerId}`, input);
}

export async function deleteProvider(providerId: number): Promise<void> {
  return api.delete<void>(`/v1/admin/llm-providers/${providerId}`);
}

export async function setDefaultProvider(providerId: number): Promise<void> {
  await api.post<void>(`/v1/admin/llm-providers/${providerId}/default`);
}
