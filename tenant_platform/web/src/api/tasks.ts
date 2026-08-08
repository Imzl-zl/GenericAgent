import { userApi } from './client';

/** GET /v1/users/me/tasks 响应项（契约 UserTask schema） */
export interface UserTask {
  task_id: string;
  session_key: string;
  status: 'queued' | 'starting' | 'running' | 'succeeded' | 'failed' | 'cancelled' | 'interrupted';
  source: 'wechat' | 'web';
  created_at: string;
  started_at?: string;
  terminal_at?: string;
  terminal_error_code?: string;
}

interface UserTaskListResponse {
  tasks: UserTask[];
}

/** GET /v1/users/me/task-stats 响应（契约 UserTaskStats schema） */
export interface UserTaskStats {
  queued: number;
  running: number;
  succeeded: number;
  failed: number;
  cancelled: number;
  interrupted: number;
  total: number;
}

/** 当前用户的最近任务（按创建时间倒序） */
export async function listMyTasks(limit = 20): Promise<UserTask[]> {
  const res = await userApi.get<UserTaskListResponse>(`/v1/users/me/tasks?limit=${limit}`);
  return res.tasks;
}

/** 当前用户的任务统计 */
export async function getMyTaskStats(): Promise<UserTaskStats> {
  return userApi.get<UserTaskStats>('/v1/users/me/task-stats');
}
