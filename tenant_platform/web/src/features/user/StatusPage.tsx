import { useEffect, useState } from 'react';
import { Card } from '../../components/ui/Card';
import { Badge } from '../../components/ui/Badge';
import { Button } from '../../components/ui/Button';
import { RefreshCw } from 'lucide-react';
import { getMyTaskStats, listMyTasks, type UserTask, type UserTaskStats } from '../../api/tasks';
import { ApiClientError } from '../../api/client';
import './UserPages.css';

const statusBadge = (status: UserTask['status']) => {
  switch (status) {
    case 'queued':
      return <Badge variant="default">排队中</Badge>;
    case 'starting':
    case 'running':
      return <Badge variant="info">运行中</Badge>;
    case 'succeeded':
      return <Badge variant="success">已完成</Badge>;
    case 'failed':
      return <Badge variant="danger">失败</Badge>;
    case 'cancelled':
      return <Badge variant="warning">已取消</Badge>;
    case 'interrupted':
      return <Badge variant="warning">已中断</Badge>;
    default:
      return <Badge variant="default">{status}</Badge>;
  }
};

const sourceLabel = (source: UserTask['source']) => (source === 'wechat' ? '微信' : '网页');

const EMPTY_STATS: UserTaskStats = {
  queued: 0,
  running: 0,
  succeeded: 0,
  failed: 0,
  cancelled: 0,
  interrupted: 0,
  total: 0,
};

export function StatusPage() {
  const [tasks, setTasks] = useState<UserTask[]>([]);
  const [stats, setStats] = useState<UserTaskStats>(EMPTY_STATS);
  const [error, setError] = useState('');
  const [isLoading, setIsLoading] = useState(true);
  const [reloadKey, setReloadKey] = useState(0);

  useEffect(() => {
    let active = true;
    void Promise.all([listMyTasks(), getMyTaskStats()])
      .then(([taskList, taskStats]) => {
        if (active) {
          setError('');
          setTasks(taskList);
          setStats(taskStats);
        }
      })
      .catch((err: unknown) => {
        if (active) {
          setError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '加载失败');
        }
      })
      .finally(() => {
        if (active) setIsLoading(false);
      });
    return () => {
      active = false;
    };
  }, [reloadKey]);

  return (
    <div className="page">
      <header className="page-header animate-fade-in-up">
        <div>
          <h1>运行状态</h1>
          <p className="page-subtitle">我的任务队列与最近任务</p>
        </div>
        <Button variant="secondary" onClick={() => setReloadKey((k) => k + 1)} disabled={isLoading}>
          <RefreshCw size={14} style={{ verticalAlign: 'middle', marginRight: 6 }} />
          刷新
        </Button>
      </header>

      {error && <span className="input-error" style={{ display: 'block', marginBottom: '12px' }}>{error}</span>}

      <div className="grid grid-4">
        <Card className="animate-fade-in-up animate-delay-1">
          <p className="metric-label">排队中</p>
          <p className="metric-value">{stats.queued}</p>
        </Card>
        <Card className="animate-fade-in-up animate-delay-2">
          <p className="metric-label">运行中</p>
          <p className="metric-value">{stats.running}</p>
        </Card>
        <Card className="animate-fade-in-up animate-delay-3">
          <p className="metric-label">已完成</p>
          <p className="metric-value">{stats.succeeded}</p>
        </Card>
        <Card className="animate-fade-in-up animate-delay-4">
          <p className="metric-label">全部任务</p>
          <p className="metric-value">{stats.total}</p>
        </Card>
      </div>

      <Card className="animate-fade-in-up animate-delay-5" style={{ marginTop: '24px' }}>
        <h3>最近任务</h3>
        <table className="data-table">
          <thead>
            <tr>
              <th>任务 ID</th>
              <th>状态</th>
              <th>来源</th>
              <th>创建时间</th>
            </tr>
          </thead>
          <tbody>
            {isLoading ? (
              <tr>
                <td colSpan={4} className="empty-cell">加载中...</td>
              </tr>
            ) : tasks.length === 0 ? (
              <tr>
                <td colSpan={4} className="empty-cell">暂无任务</td>
              </tr>
            ) : (
              tasks.map((task) => (
                <tr key={task.task_id}>
                  <td><code>{task.task_id.slice(0, 12)}</code></td>
                  <td>{statusBadge(task.status)}</td>
                  <td>{sourceLabel(task.source)}</td>
                  <td>{new Date(task.created_at).toLocaleString()}</td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </Card>
    </div>
  );
}
