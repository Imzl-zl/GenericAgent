import { api } from './client';
import type { BindingCode } from './types';

export async function createBinding(userId: number): Promise<BindingCode> {
  return api.post<BindingCode>('/v1/bindings', { user_id: userId });
}
