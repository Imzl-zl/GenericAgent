import { useEffect, useState } from 'react';
import { Card } from '../../components/ui/Card';
import { Button } from '../../components/ui/Button';
import { Badge } from '../../components/ui/Badge';
import { Copy, Plus, Trash2 } from 'lucide-react';
import { createInviteCode, listInviteCodes, revokeInviteCode } from '../../api/invite';
import { ApiClientError } from '../../api/client';
import type { InviteCode } from '../../api/types';
import './AdminPages.css';

const stateBadge = (state: string) => {
  if (state === 'active') return <Badge variant="success">有效</Badge>;
  if (state === 'used') return <Badge variant="info">已用</Badge>;
  if (state === 'revoked') return <Badge variant="danger">已撤销</Badge>;
  return <Badge variant="warning">过期</Badge>;
};

export function InviteCodesPage() {
  const [codes, setCodes] = useState<InviteCode[]>([]);
  const [error, setError] = useState('');
  const [isLoading, setIsLoading] = useState(true);
  const [generated, setGenerated] = useState<InviteCode | null>(null);

  const loadCodes = async () => {
    setIsLoading(true);
    setError('');
    try {
      const list = await listInviteCodes();
      setCodes(list);
    } catch (err) {
      setError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '加载失败');
    } finally {
      setIsLoading(false);
    }
  };

  useEffect(() => {
    loadCodes();
  }, []);

  const handleGenerate = async () => {
    setError('');
    try {
      const code = await createInviteCode();
      setGenerated(code);
      await loadCodes();
    } catch (err) {
      setError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '生成失败');
    }
  };

  const handleRevoke = async (code: string) => {
    setError('');
    try {
      await revokeInviteCode(code);
      await loadCodes();
    } catch (err) {
      setError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '撤销失败');
    }
  };

  const copyCode = (code: string) => {
    navigator.clipboard.writeText(code);
  };

  return (
    <div className="admin-page">
      <header className="admin-header animate-fade-in-up">
        <div>
          <h1>邀请码</h1>
          <p className="admin-subtitle">生成、撤销与管理一次性邀请码</p>
        </div>
        <div className="admin-actions">
          <Button variant="secondary" onClick={loadCodes} disabled={isLoading}>刷新</Button>
          <Button onClick={handleGenerate}>
            <Plus size={16} />
            生成邀请码
          </Button>
        </div>
      </header>

      {generated && (
        <Card className="animate-fade-in-up animate-delay-1" style={{ marginBottom: '24px' }}>
          <div className="admin-metric-label">新生成的邀请码</div>
          <div style={{ display: 'flex', alignItems: 'center', gap: '12px', marginTop: '8px' }}>
            <code style={{ fontSize: '20px' }}>{generated.code}</code>
            <button className="icon-button" type="button" aria-label="复制" onClick={() => copyCode(generated.code)}>
              <Copy size={16} />
            </button>
          </div>
          <p className="admin-subtitle" style={{ marginTop: '8px' }}>
            过期时间：{new Date(generated.expires_at).toLocaleString()}
          </p>
        </Card>
      )}

      <Card className="animate-fade-in-up animate-delay-2">
        {error && <span className="input-error" style={{ display: 'block', marginBottom: '12px' }}>{error}</span>}
        <table className="admin-table">
          <thead>
            <tr>
              <th>邀请码</th>
              <th>状态</th>
              <th>使用者</th>
              <th>过期时间</th>
              <th>创建时间</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {isLoading ? (
              <tr>
                <td colSpan={6} className="admin-empty">加载中...</td>
              </tr>
            ) : codes.length === 0 ? (
              <tr>
                <td colSpan={6} className="admin-empty">暂无邀请码</td>
              </tr>
            ) : (
              codes.map((ic) => (
                <tr key={ic.code}>
                  <td>
                    <code>{ic.code}</code>
                    <button className="icon-button" type="button" aria-label="复制" onClick={() => copyCode(ic.code)}>
                      <Copy size={14} />
                    </button>
                  </td>
                  <td>{stateBadge(ic.state)}</td>
                  <td>{ic.used_by ?? '-'}</td>
                  <td>{new Date(ic.expires_at).toLocaleString()}</td>
                  <td>{new Date(ic.created_at).toLocaleString()}</td>
                  <td>
                    {ic.state === 'active' && (
                      <Button variant="danger" onClick={() => handleRevoke(ic.code)}>
                        <Trash2 size={14} />
                        撤销
                      </Button>
                    )}
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
