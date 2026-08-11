import { Fragment, useEffect, useState } from 'react';
import { Card } from '../../components/ui/Card';
import { Button } from '../../components/ui/Button';
import { Badge } from '../../components/ui/Badge';
import { listPendingUsers, approveUser, blockUser } from '../../api/users';
import { deleteMCPQuota, listMCPQuotas, listMCPServers, upsertMCPQuota } from '../../api/mcpServers';
import { ApiClientError } from '../../api/client';
import type { MCPQuotaLimit, User } from '../../api/types';
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
    let active = true;
    void listPendingUsers()
      .then((list) => {
        if (active) setUsers(list);
      })
      .catch((err: unknown) => {
        if (active) setError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '加载失败');
      })
      .finally(() => {
        if (active) setIsLoading(false);
      });
    return () => {
      active = false;
    };
  }, []);

  const [expanded, setExpanded] = useState<number>();
  const [quotas, setQuotas] = useState<Record<number, MCPQuotaLimit[]>>({});
  const [quotaError, setQuotaError] = useState('');
  const [servers, setServers] = useState<{ mcp_server_id: number; server_key: string }[]>([]);

  const toggleQuota = async (userId: number) => {
    setQuotaError('');
    if (expanded === userId) {
      setExpanded(undefined);
      return;
    }
    setExpanded(userId);
    try {
      if (servers.length === 0) {
        const list = await listMCPServers();
        setServers(list.map((srv) => ({ mcp_server_id: srv.mcp_server_id, server_key: srv.server_key })));
      }
      const ownerKey = String(userId);
      const list = await listMCPQuotas(ownerKey);
      setQuotas((prev) => ({ ...prev, [userId]: list }));
    } catch (err) {
      setQuotaError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '加载配额失败');
    }
  };

  const saveQuota = async (userId: number, serverId: string, period: 'day' | 'month', limit: number) => {
    setQuotaError('');
    if (!serverId || !limit || limit <= 0) {
      setQuotaError('请选择 MCP Server 并填写大于 0 的限额');
      return;
    }
    try {
      await upsertMCPQuota({ owner_key: String(userId), server_id: serverId, period, limit_count: limit });
      const list = await listMCPQuotas(String(userId));
      setQuotas((prev) => ({ ...prev, [userId]: list }));
    } catch (err) {
      setQuotaError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '保存配额失败');
    }
  };

  const removeQuota = async (userId: number, serverId: string, period: string) => {
    setQuotaError('');
    try {
      await deleteMCPQuota(String(userId), serverId, period);
      const list = await listMCPQuotas(String(userId));
      setQuotas((prev) => ({ ...prev, [userId]: list }));
    } catch (err) {
      setQuotaError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '删除配额失败');
    }
  };

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
              <th>MCP 配额</th>
            </tr>
          </thead>
          <tbody>
            {isLoading ? (
              <tr>
                <td colSpan={6} className="admin-empty">加载中...</td>
              </tr>
            ) : users.length === 0 ? (
              <tr>
                <td colSpan={6} className="admin-empty">暂无待审批用户</td>
              </tr>
            ) : (
              users.map((user) => (
                <Fragment key={user.user_id}>
                <tr>
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
                  <td>
                    {user.status === 'approved' && (
                      <Button variant="secondary" onClick={() => void toggleQuota(user.user_id)}>
                        {expanded === user.user_id ? '收起' : '配额'}
                      </Button>
                    )}
                  </td>
                </tr>
                {expanded === user.user_id && (
                  <tr>
                    <td colSpan={6}>
                      <QuotaPanel
                        userId={user.user_id}
                        servers={servers}
                        quotas={quotas[user.user_id] ?? []}
                        error={quotaError}
                        onSave={saveQuota}
                        onDelete={removeQuota}
                      />
                    </td>
                  </tr>
                )}
                </Fragment>
              ))
            )}
          </tbody>
        </table>
      </Card>
    </div>
  );
}

interface QuotaPanelProps {
  userId: number;
  servers: { mcp_server_id: number; server_key: string }[];
  quotas: MCPQuotaLimit[];
  error: string;
  onSave: (userId: number, serverId: string, period: 'day' | 'month', limit: number) => void;
  onDelete: (userId: number, serverId: string, period: string) => void;
}

function QuotaPanel({ userId, servers, quotas, error, onSave, onDelete }: QuotaPanelProps) {
  const [serverId, setServerId] = useState('');
  const [period, setPeriod] = useState<'day' | 'month'>('day');
  const [limit, setLimit] = useState(100);
  return (
    <div className="quota-panel">
      <div className="quota-form">
        <select className="input-field" value={serverId} onChange={(e) => setServerId(e.target.value)}>
          <option value="">选择 MCP Server...</option>
          {servers.map((srv) => (
            <option key={srv.mcp_server_id} value={srv.server_key}>{srv.server_key}</option>
          ))}
        </select>
        <select className="input-field" value={period} onChange={(e) => setPeriod(e.target.value as 'day' | 'month')}>
          <option value="day">每日</option>
          <option value="month">每月</option>
        </select>
        <input
          className="input-field"
          type="number"
          min={1}
          value={limit}
          onChange={(e) => setLimit(Number(e.target.value))}
          placeholder="限额"
        />
        <Button onClick={() => onSave(userId, serverId, period, limit)}>设置限额</Button>
      </div>
      {error && <span className="input-error" style={{ display: 'block', marginTop: '8px' }}>{error}</span>}
      {quotas.length > 0 && (
        <table className="admin-table quota-table">
          <thead>
            <tr>
              <th>Server</th>
              <th>周期</th>
              <th>限额</th>
              <th aria-label="操作" />
            </tr>
          </thead>
          <tbody>
            {quotas.map((q) => (
              <tr key={`${q.server_id}-${q.period}`}>
                <td>{q.server_id}</td>
                <td>{q.period === 'day' ? '每日' : '每月'}</td>
                <td>{q.limit_count}</td>
                <td>
                  <Button variant="danger" onClick={() => onDelete(userId, q.server_id, q.period)}>删除</Button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {quotas.length === 0 && !error && <p className="admin-empty">未设置配额（默认放行）</p>}
    </div>
  );
}
