import { api } from './client';

export interface RuntimeProfile {
  claim_lease_seconds: number;
  token_ttl_seconds: number;
  token_refresh_skew_seconds: number;
  max_task_wall_clock_seconds: number;
  task_timeout_seconds: number;
  task_idle_timeout_seconds: number;
  worker_idle_ttl_seconds: number;
  max_running_tasks: number;
  per_tenant_running_limit: number;
  per_user_queue_limit: number;
  im_inbound_coalesce_window_ms: number;
  agent_max_turns: number;
}

export interface DashboardStats {
  pending_users: number;
  approved_users: number;
  running_tasks: number;
  active_workers: number;
  runtime_profile: RuntimeProfile;
}

export async function getDashboardStats(): Promise<DashboardStats> {
  return api.get<DashboardStats>('/v1/admin/dashboard/stats');
}
