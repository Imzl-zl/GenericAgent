import { api } from './client';
import type { LLMProvider } from './types';

interface ProviderListResponse { providers: LLMProvider[] }

export interface CreateProviderInput {
  name: string;
  provider_type: string;
  base_url: string;
  model: string;
  api_key: string;
}

export async function listProviders(): Promise<LLMProvider[]> {
  const res = await api.get<ProviderListResponse>('/v1/admin/llm-providers');
  return res.providers;
}

export async function createProvider(input: CreateProviderInput): Promise<LLMProvider> {
  return api.post<LLMProvider>('/v1/admin/llm-providers', input);
}

export async function deleteProvider(providerId: number): Promise<void> {
  return api.delete<void>(`/v1/admin/llm-providers/${providerId}`);
}

export async function setDefaultProvider(providerId: number): Promise<void> {
  await api.post<void>(`/v1/admin/llm-providers/${providerId}/default`);
}
