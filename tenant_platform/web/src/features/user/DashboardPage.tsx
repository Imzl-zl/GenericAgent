import { useEffect, useState } from 'react';
import { Card } from '../../components/ui/Card';
import { Badge } from '../../components/ui/Badge';
import { useAuth } from '../../contexts/AuthContext';
import { listPendingUsers } from '../../api/users';
import type { User } from '../../api/types';
import './UserPages.css';

export function DashboardPage() {
  const { state } = useAuth();
  const [currentUser, setCurrentUser] = useState<User | null>(null);
  const [isLoading, setIsLoading] = useState(true);

  useEffect(() => {
    const loadStatus = async () => {
      try {
        const pending = await listPendingUsers();
        const found = pending.find((u) => u.username === state?.username);
        if (found) {
          setCurrentUser(found);
        } else {
          setCurrentUser({
            user_id: Number(localStorage.getItem('ga_user_id') || 0),
            username: state?.username || '',
            status: 'approved',
            created_at: '',
          });
        }
      } finally {
        setIsLoading(false);
      }
    };
    loadStatus();
  }, [state?.username]);

  const statusVariant = currentUser?.status === 'approved' ? 'success' : 'warning';
  const statusText = currentUser?.status === 'approved' ? '已批准' : '待批准';

  return (
    <div className="page">
      <header className="page-header animate-fade-in-up">
        <div>
          <h1>仪表盘</h1>
          <p className="page-subtitle">个人工作区概览</p>
        </div>
        {!isLoading && <Badge variant={statusVariant}>{statusText}</Badge>}
      </header>

      <div className="grid grid-3">
        <Card className="animate-fade-in-up animate-delay-1">
          <h3>账号状态</h3>
          <p className="metric-value muted">{isLoading ? '...' : currentUser?.status.toUpperCase()}</p>
          <p className="page-subtitle">
            {currentUser?.status === 'approved'
              ? '账号已激活，可发起微信绑定'
              : '等待平台运营者人工批准'}
          </p>
        </Card>
        <Card className="animate-fade-in-up animate-delay-2">
          <h3>Bot 绑定</h3>
          <p className="metric-value muted">未绑定</p>
          <p className="page-subtitle">批准后在微信绑定页发起</p>
        </Card>
        <Card className="animate-fade-in-up animate-delay-3">
          <h3>最近任务</h3>
          <p className="metric-value muted">0</p>
          <p className="page-subtitle">当前无运行中任务</p>
        </Card>
      </div>

      <Card className="animate-fade-in-up animate-delay-4" style={{ marginTop: '24px' }}>
        <h3>下一步</h3>
        <ol className="todo-list">
          <li>等待运营者批准账号</li>
          <li>完成微信 bot 绑定</li>
          <li>编辑个人默认人设</li>
          <li>在微信中开始对话</li>
        </ol>
      </Card>
    </div>
  );
}
