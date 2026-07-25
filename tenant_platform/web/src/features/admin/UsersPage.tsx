import { useEffect, useState } from 'react';
import { Card } from '../../components/ui/Card';
import { Button } from '../../components/ui/Button';
import { Badge } from '../../components/ui/Badge';
import { listPendingUsers, approveUser, blockUser } from '../../api/users';
import { ApiClientError } from '../../api/client';
import type { User } from '../../api/types';
import './AdminPages.css';

const statusBadge = (status: string) => {
  if (status === 'approved') return <Badge variant="success">已批准</Badge>;
  if (status === 'pending') return <Badge variant="warning">待审批</Badge>;
  return <Badge variant="danger">已封禁</Badge>;
};

export function UsersPage() {
  const [users, setUsers] = useState<User[]>([]);
  const [error, setError] = useState('');
  const [isLoading, setIsLoading] = useState(true);

  const loadUsers = async () => {
    setIsLoading(true);
    setError('');
    try {
      const pending = await listPendingUsers();
      setUsers(pending);
    } catch (err) {
      setError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '加载失败');
    } finally {
      setIsLoading(false);
    }
  };

  useEffect(() => {
    loadUsers();
  }, []);

  const handleApprove = async (userId: number) => {
    try {
      await approveUser(userId);
      await loadUsers();
    } catch (err) {
      setError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '审批失败');
    }
  };

  return (
    <div className="admin-page">
      <header className="admin-header animate-fade-in-up">
        <div>
          <h1>用户审批</h1>
          <p className="admin-subtitle">批准、封禁与管理平台用户</p>
        </div>
        <Button variant="secondary" onClick={loadUsers} disabled={isLoading}>刷新</Button>
      </header>

      <Card className="animate-fade-in-up animate-delay-1">
        {error && <span className="input-error" style={{ display: 'block', marginBottom: '12px' }}>{error}</span>}
        <table className="admin-table">
          <thead>
            <tr>
              <th>ID</th>
              <th>用户名</th>
              <th>状态</th>
              <th>注册时间</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {isLoading ? (
              <tr>
                <td colSpan={5} className="admin-empty">加载中...</td>
              </tr>
            ) : users.length === 0 ? (
              <tr>
                <td colSpan={5} className="admin-empty">暂无待审批用户</td>
              </tr>
            ) : (
              users.map((user) => (
                <tr key={user.user_id}>
                  <td>{user.user_id}</td>
                  <td>{user.username}</td>
                  <td>{statusBadge(user.status)}</td>
                  <td>{new Date(user.created_at).toLocaleString()}</td>
                  <td>
                    <div className="admin-actions">
                      {user.status === 'pending' && (
                        <Button onClick={() => handleApprove(user.user_id)}>批准</Button>
                      )}
                      {user.status === 'approved' && (
                        <Button variant="danger" onClick={() => blockUser(user.user_id)}>封禁</Button>
                      )}
                    </div>
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </Card>
    </div>
  );
}
