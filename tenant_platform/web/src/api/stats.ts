import { api } from './client';

export interface DashboardStats {
  pending_users: number;
  approved_users: number;
  running_tasks: number;
  active_workers: number;
}

export async function getDashboardStats(): Promise<DashboardStats> {
  return api.get<DashboardStats>('/v1/admin/dashboard/stats');
}
