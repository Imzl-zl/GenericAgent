import { api } from './client';
import type { InviteCode, RegisterResponse } from './types';

interface RegisterRequest {
  username: string;
  password: string;
  invite_code: string;
}

interface LoginRequest {
  username: string;
  password: string;
}

interface InviteCodeListResponse {
  invite_codes: InviteCode[];
}

export async function register(username: string, password: string, inviteCode: string): Promise<RegisterResponse> {
  return api.post<RegisterResponse>('/v1/register', { username, password, invite_code: inviteCode } as RegisterRequest);
}

export async function login(username: string, password: string): Promise<RegisterResponse> {
  return api.post<RegisterResponse>('/v1/login', { username, password } as LoginRequest);
}

export async function createInviteCode(): Promise<InviteCode> {
  return api.post<InviteCode>('/v1/admin/invite-codes');
}

export async function listInviteCodes(): Promise<InviteCode[]> {
  const res = await api.get<InviteCodeListResponse>('/v1/admin/invite-codes');
  return res.invite_codes;
}

export async function revokeInviteCode(code: string): Promise<{ revoked: boolean }> {
  return api.delete<{ revoked: boolean }>(`/v1/admin/invite-codes/${encodeURIComponent(code)}`);
}
