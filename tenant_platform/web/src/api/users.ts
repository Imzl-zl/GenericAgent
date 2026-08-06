import { api } from './client';
import type { User } from './types';

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
