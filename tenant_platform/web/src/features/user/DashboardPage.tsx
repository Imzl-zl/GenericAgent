import { useEffect } from 'react';
import { Card } from '../../components/ui/Card';
import { Badge } from '../../components/ui/Badge';
import { useAuth } from '../../contexts/AuthContext';
import './UserPages.css';

export function DashboardPage() {
  const { state, refreshStatus } = useAuth();

  // 管理员审批通过后，登录时缓存的状态快照已过期——挂载时从后端拉取
  // 实时状态，避免页面一直显示"待批准"直到重新登录。
  // 待批准状态下轻量轮询：审批通过后页面自动更新，无需手动刷新。
  useEffect(() => {
    refreshStatus().catch(() => undefined);
    if (state?.status !== 'approved') {
      const timer = window.setInterval(() => {
        refreshStatus().catch(() => undefined);
      }, 20000);
      return () => window.clearInterval(timer);
    }
  }, [refreshStatus, state?.status]);

  // 审查 F5: 普通用户 Dashboard 不再调用管理员 /v1/admin/users/pending
  // (必然 401)。当前用户状态直接来自登录/注册响应(AuthContext), 由后端
  // 在签发 token 时返回。
  const status: 'pending' | 'approved' | 'blocked' = state?.status ?? 'pending';
  const userId = state?.userId ?? 0;

  const statusVariant = status === 'approved' ? 'success' : 'warning';
  const statusText = status === 'approved' ? '已批准' : status === 'blocked' ? '已封禁' : '待批准';

  return (
    <div className="page">
      <h1>我的控制台</h1>
      <Card className="dashboard-card">
        <div className="dashboard-row">
          <span className="dashboard-label">用户名</span>
          <span>{state?.username ?? '-'}</span>
        </div>
        <div className="dashboard-row">
          <span className="dashboard-label">用户 ID</span>
          <span>{userId > 0 ? userId : '-'}</span>
        </div>
        <div className="dashboard-row">
          <span className="dashboard-label">账号状态</span>
          <Badge variant={statusVariant}>{statusText}</Badge>
        </div>
      </Card>
    </div>
  );
}
