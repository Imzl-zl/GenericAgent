import { api, userApi } from './client';
import type { MeResponse, User } from './types';

/** 当前登录用户实时状态（审批后刷新用，userAuth 保护） */
export async function getMe(): Promise<MeResponse> {
  return userApi.get<MeResponse>('/v1/users/me');
}

interface PendingListResponse { users: User[] }

export async function approveUser(userId: number): Promise<User> {
  return api.post<User>(`/v1/admin/users/${userId}/approve`);
}

export async function blockUser(userId: number): Promise<User> {
  return api.post<User>(`/v1/admin/users/${userId}/block`);
}

export async function listPendingUsers(): Promise<User[]> {
  const res = await api.get<PendingListResponse>('/v1/admin/users/pending');
  return res.users;
}
