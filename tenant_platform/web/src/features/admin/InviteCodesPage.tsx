import { useEffect, useRef, useState } from 'react';
import { Ban, Copy, Plus, Trash2 } from 'lucide-react';
import { createInviteCode, deleteInviteCodes, listInviteCodes, revokeInviteCode } from '../../api/invite';
import { ApiClientError } from '../../api/client';
import type { InviteCode } from '../../api/types';
import { Badge } from '../../components/ui/Badge';
import { Button } from '../../components/ui/Button';
import { Card } from '../../components/ui/Card';
import './AdminPages.css';

const stateBadge = (state: string) => {
  if (state === 'active') return <Badge variant="success">有效</Badge>;
  if (state === 'used') return <Badge variant="info">已用</Badge>;
  if (state === 'revoked') return <Badge variant="danger">已撤销</Badge>;
  return <Badge variant="warning">过期</Badge>;
};

const errorMessage = (error: unknown, fallback: string) => (
  error instanceof ApiClientError ? `${error.code}: ${error.message}` : fallback
);

export function InviteCodesPage() {
  const [codes, setCodes] = useState<InviteCode[]>([]);
  const [selectedCodes, setSelectedCodes] = useState<Set<string>>(new Set());
  const [error, setError] = useState('');
  const [message, setMessage] = useState('');
  const [isLoading, setIsLoading] = useState(true);
  const [isDeleting, setIsDeleting] = useState(false);
  const [generated, setGenerated] = useState<InviteCode | null>(null);
  const selectAllRef = useRef<HTMLInputElement>(null);

  const selectedCount = selectedCodes.size;
  const allSelected = codes.length > 0 && selectedCount === codes.length;

  useEffect(() => {
    if (selectAllRef.current) {
      selectAllRef.current.indeterminate = selectedCount > 0 && !allSelected;
    }
  }, [allSelected, selectedCount]);

  const loadCodes = async (): Promise<boolean> => {
    setIsLoading(true);
    setError('');
    try {
      const list = await listInviteCodes();
      const availableCodes = new Set(list.map((item) => item.code));
      setCodes(list);
      setSelectedCodes((current) => new Set([...current].filter((code) => availableCodes.has(code))));
      return true;
    } catch (loadError) {
      setError(errorMessage(loadError, '加载失败'));
      return false;
    } finally {
      setIsLoading(false);
    }
  };

  useEffect(() => {
    let active = true;
    void listInviteCodes()
      .then((list) => {
        if (active) setCodes(list);
      })
      .catch((loadError: unknown) => {
        if (active) setError(errorMessage(loadError, '加载失败'));
      })
      .finally(() => {
        if (active) setIsLoading(false);
      });
    return () => {
      active = false;
    };
  }, []);

  const handleGenerate = async () => {
    setError('');
    setMessage('');
    try {
      const code = await createInviteCode();
      setGenerated(code);
      await loadCodes();
    } catch (generateError) {
      setError(errorMessage(generateError, '生成失败'));
    }
  };

  const handleRevoke = async (code: string) => {
    setError('');
    setMessage('');
    try {
      await revokeInviteCode(code);
      await loadCodes();
      setMessage(`已撤销邀请码 ${code}`);
    } catch (revokeError) {
      setError(errorMessage(revokeError, '撤销失败'));
    }
  };

  const handleDelete = async (codesToDelete: string[]) => {
    const prompt = codesToDelete.length === 1
      ? `确定永久删除邀请码“${codesToDelete[0]}”？此操作不可恢复。`
      : `确定永久删除选中的 ${codesToDelete.length} 个邀请码？此操作不可恢复。`;
    if (!window.confirm(prompt)) return;

    setError('');
    setMessage('');
    setIsDeleting(true);
    try {
      const result = await deleteInviteCodes(codesToDelete);
      const deletedSet = new Set(codesToDelete);
      setCodes((current) => current.filter((item) => !deletedSet.has(item.code)));
      setSelectedCodes((current) => new Set([...current].filter((code) => !deletedSet.has(code))));
      setGenerated((current) => current && deletedSet.has(current.code) ? null : current);
      const refreshed = await loadCodes();
      setMessage(
        refreshed
          ? `已永久删除 ${result.deleted} 个邀请码`
          : `已永久删除 ${result.deleted} 个邀请码，但列表刷新失败`,
      );
    } catch (deleteError) {
      setError(errorMessage(deleteError, '删除失败'));
    } finally {
      setIsDeleting(false);
    }
  };

  const toggleCode = (code: string) => {
    setSelectedCodes((current) => {
      const next = new Set(current);
      if (next.has(code)) next.delete(code);
      else next.add(code);
      return next;
    });
  };

  const toggleAll = () => {
    setSelectedCodes(allSelected ? new Set() : new Set(codes.map((item) => item.code)));
  };

  const copyCode = (code: string) => {
    void navigator.clipboard.writeText(code);
  };

  return (
    <div className="admin-page invite-codes-page">
      <header className="admin-header animate-fade-in-up">
        <div>
          <h1>邀请码</h1>
          <p className="admin-subtitle">生成、撤销与清理一次性邀请码</p>
        </div>
        <div className="admin-actions">
          <Button variant="secondary" onClick={() => void loadCodes()} disabled={isLoading || isDeleting}>刷新</Button>
          <Button onClick={() => void handleGenerate()} disabled={isDeleting}>
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
            <button className="icon-button" type="button" title="复制" aria-label="复制" onClick={() => copyCode(generated.code)}>
              <Copy size={16} />
            </button>
          </div>
          <p className="admin-subtitle" style={{ marginTop: '8px' }}>
            过期时间：{new Date(generated.expires_at).toLocaleString()}
          </p>
        </Card>
      )}

      {message && <div className="admin-success" role="status">{message}</div>}

      <Card className="animate-fade-in-up animate-delay-2">
        {error && <div className="admin-error" role="alert">{error}</div>}
        <div className="invite-bulk-toolbar">
          <span className="invite-selection-count">已选 {selectedCount} 项</span>
          <Button
            variant="danger"
            isLoading={isDeleting}
            disabled={selectedCount === 0 || isLoading}
            onClick={() => void handleDelete([...selectedCodes])}
          >
            <Trash2 size={15} />
            删除选中
          </Button>
        </div>
        <div className="invite-table-scroll">
          <table className="admin-table invite-table">
            <thead>
              <tr>
                <th className="invite-select-cell">
                  <input
                    ref={selectAllRef}
                    type="checkbox"
                    aria-label="选择全部邀请码"
                    checked={allSelected}
                    disabled={isLoading || isDeleting || codes.length === 0}
                    onChange={toggleAll}
                  />
                </th>
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
                  <td colSpan={7} className="admin-empty">加载中...</td>
                </tr>
              ) : codes.length === 0 ? (
                <tr>
                  <td colSpan={7} className="admin-empty">暂无邀请码</td>
                </tr>
              ) : (
                codes.map((ic) => (
                  <tr key={ic.code} className={selectedCodes.has(ic.code) ? 'is-selected' : undefined}>
                    <td className="invite-select-cell">
                      <input
                        type="checkbox"
                        aria-label={`选择邀请码 ${ic.code}`}
                        checked={selectedCodes.has(ic.code)}
                        disabled={isDeleting}
                        onChange={() => toggleCode(ic.code)}
                      />
                    </td>
                    <td className="invite-code-cell">
                      <code>{ic.code}</code>
                      <button className="icon-button" type="button" title="复制" aria-label={`复制邀请码 ${ic.code}`} onClick={() => copyCode(ic.code)}>
                        <Copy size={14} />
                      </button>
                    </td>
                    <td>{stateBadge(ic.state)}</td>
                    <td>{ic.used_by ?? '-'}</td>
                    <td>{new Date(ic.expires_at).toLocaleString()}</td>
                    <td>{new Date(ic.created_at).toLocaleString()}</td>
                    <td>
                      <div className="admin-actions invite-row-actions">
                        {ic.state === 'active' && (
                          <Button variant="danger" disabled={isDeleting} onClick={() => void handleRevoke(ic.code)}>
                            <Ban size={14} />
                            撤销
                          </Button>
                        )}
                        <button
                          className="icon-button danger"
                          type="button"
                          title="永久删除"
                          aria-label={`永久删除邀请码 ${ic.code}`}
                          disabled={isDeleting}
                          onClick={() => void handleDelete([ic.code])}
                        >
                          <Trash2 size={16} />
                        </button>
                      </div>
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      </Card>
    </div>
  );
}
