import { useEffect, useState } from 'react';
import { Card } from '../../components/ui/Card';
import { Button } from '../../components/ui/Button';
import { Input } from '../../components/ui/Input';
import { Badge } from '../../components/ui/Badge';
import { Check, X, Edit2, Plus, RotateCcw, Star, Trash2 } from 'lucide-react';
import {
  listPendingPersonas,
  approvePersona,
  rejectPersona,
  adminListPersonas,
  adminListMyPersonas,
  adminCreatePersona,
  adminUpdatePersona,
  adminDeletePersona,
  adminSetDefaultPersona,
  adminClearDefaultPersona,
} from '../../api/personas';
import { ApiClientError } from '../../api/client';
import type { Persona } from '../../api/types';
import './AdminPages.css';

const DEFAULT_PERSONA_PROMPT = [
  '你是一位严谨的工程助手，擅长：',
  '- 阅读和分析技术文档',
  '- 在隔离环境中运行 Shell/Python',
  '- 给出可验证、可追溯的回答',
  '',
  '回答问题时优先引用工作区文件内容，不确定时明确说明。',
].join('\n');

type Tab = 'pending' | 'pool' | 'mine';

const statusBadge = (status: string) => {
  if (status === 'approved') return <Badge variant="success">已通过</Badge>;
  if (status === 'pending') return <Badge variant="warning">审核中</Badge>;
  if (status === 'rejected') return <Badge variant="danger">已拒绝</Badge>;
  return <Badge variant="default">私有</Badge>;
};

const errText = (err: unknown, fallback: string) =>
  err instanceof ApiClientError ? `${err.code}: ${err.message}` : fallback;

async function fetchPersonaCollections() {
  const [pending, pool, mine] = await Promise.all([
    listPendingPersonas(),
    adminListPersonas(),
    adminListMyPersonas(),
  ]);
  return { pending, pool, mine };
}

export function PersonaReviewPage() {
  const [tab, setTab] = useState<Tab>('pending');
  const [pending, setPending] = useState<Persona[]>([]);
  const [pool, setPool] = useState<Persona[]>([]);
  const [mine, setMine] = useState<Persona[]>([]);
  const [notes, setNotes] = useState<Record<string, string>>({});
  const [defaultId, setDefaultId] = useState<string>(
    () => localStorage.getItem('ga_admin_default_persona_id') || ''
  );
  const [error, setError] = useState('');
  const [success, setSuccess] = useState('');
  const [isLoading, setIsLoading] = useState(true);

  // 管理员自建人设表单
  const [editingId, setEditingId] = useState<string | null>(null);
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [systemPrompt, setSystemPrompt] = useState(DEFAULT_PERSONA_PROMPT);
  const [isPublic, setIsPublic] = useState(false);
  const [isSaving, setIsSaving] = useState(false);

  const loadAll = async () => {
    setIsLoading(true);
    setError('');
    try {
      const collections = await fetchPersonaCollections();
      setPending(collections.pending);
      setPool(collections.pool);
      setMine(collections.mine);
    } catch (err) {
      setError(errText(err, '加载失败'));
    } finally {
      setIsLoading(false);
    }
  };

  useEffect(() => {
    let active = true;
    void fetchPersonaCollections()
      .then((collections) => {
        if (!active) return;
        setPending(collections.pending);
        setPool(collections.pool);
        setMine(collections.mine);
      })
      .catch((err: unknown) => {
        if (active) setError(errText(err, '加载失败'));
      })
      .finally(() => {
        if (active) setIsLoading(false);
      });
    return () => {
      active = false;
    };
  }, []);

  const handleModerate = async (id: string, approve: boolean) => {
    setError('');
    setSuccess('');
    try {
      if (approve) {
        await approvePersona(id, notes[id] || '');
      } else {
        await rejectPersona(id, notes[id] || '');
      }
      // 下架后服务端会清理 default_persona_id，前端同步清理
      if (!approve && defaultId === id) {
        setDefaultId('');
        localStorage.removeItem('ga_admin_default_persona_id');
      }
      setSuccess(approve ? '已通过' : '已拒绝/下架');
      await loadAll();
    } catch (err) {
      setError(errText(err, '审核失败'));
    }
  };

  const resetForm = () => {
    setEditingId(null);
    setName('');
    setDescription('');
    setSystemPrompt(DEFAULT_PERSONA_PROMPT);
    setIsPublic(false);
  };

  const startEdit = (p: Persona) => {
    setTab('mine');
    setEditingId(p.id);
    setName(p.name);
    setDescription(p.description);
    setSystemPrompt(p.system_prompt);
    setIsPublic(p.is_public);
  };

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault();
    setError('');
    setSuccess('');
    if (!name.trim() || !systemPrompt.trim()) {
      setError('名称和系统提示词必填');
      return;
    }
    setIsSaving(true);
    try {
      if (editingId) {
        await adminUpdatePersona(editingId, name.trim(), description.trim(), systemPrompt.trim());
        setSuccess('更新成功');
      } else {
        await adminCreatePersona(name.trim(), description.trim(), systemPrompt.trim(), isPublic);
        setSuccess(isPublic ? '已创建并发布到公共池' : '创建成功');
      }
      resetForm();
      await loadAll();
    } catch (err) {
      setError(errText(err, '保存失败'));
    } finally {
      setIsSaving(false);
    }
  };

  const handleDelete = async (id: string) => {
    if (!window.confirm('确定从公共池删除这个人设？将同时清除所有把它设为默认的用户。')) return;
    setError('');
    setSuccess('');
    try {
      await adminDeletePersona(id);
      if (defaultId === id) {
        setDefaultId('');
        localStorage.removeItem('ga_admin_default_persona_id');
      }
      setSuccess('已删除');
      await loadAll();
    } catch (err) {
      setError(errText(err, '删除失败'));
    }
  };

  const handleSetDefault = async (id: string) => {
    setError('');
    setSuccess('');
    try {
      await adminSetDefaultPersona(id);
      setDefaultId(id);
      localStorage.setItem('ga_admin_default_persona_id', id);
      setSuccess('已设为默认');
    } catch (err) {
      setError(errText(err, '设置失败'));
    }
  };

  const handleClearDefault = async () => {
    setError('');
    setSuccess('');
    try {
      await adminClearDefaultPersona();
      setDefaultId('');
      localStorage.removeItem('ga_admin_default_persona_id');
      setSuccess('已取消默认');
    } catch (err) {
      setError(errText(err, '取消失败'));
    }
  };

  const tabs: { key: Tab; label: string }[] = [
    { key: 'pending', label: `待审核 (${pending.length})` },
    { key: 'pool', label: `公共池 (${pool.filter((p) => p.status === 'approved').length})` },
    { key: 'mine', label: `我的人设 (${mine.length})` },
  ];

  return (
    <div className="admin-page">
      <header className="admin-header animate-fade-in-up">
        <div>
          <h1>人设管理</h1>
          <p className="admin-subtitle">审核提交、管理公共池、创建管理员自己的人设</p>
        </div>
        <Button variant="secondary" onClick={loadAll} disabled={isLoading}>刷新</Button>
      </header>

      <div style={{ display: 'flex', gap: '8px', marginBottom: '20px' }}>
        {tabs.map((t) => (
          <Button
            key={t.key}
            variant={tab === t.key ? 'primary' : 'secondary'}
            onClick={() => setTab(t.key)}
          >
            {t.label}
          </Button>
        ))}
      </div>

      {error && <p className="input-error" style={{ marginBottom: '12px' }}>{error}</p>}
      {success && <p className="input-error" style={{ color: 'var(--accent)', marginBottom: '12px' }}>{success}</p>}

      {tab === 'pending' && (
        <Card className="animate-fade-in-up">
          {isLoading ? (
            <p className="admin-empty">加载中...</p>
          ) : pending.length === 0 ? (
            <p className="admin-empty">暂无待审核人设</p>
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column', gap: '20px' }}>
              {pending.map((p) => (
                <div key={p.id} className="persona-review-item">
                  <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '12px' }}>
                    <div style={{ display: 'flex', alignItems: 'center', gap: '10px' }}>
                      <strong style={{ fontSize: '16px' }}>{p.name}</strong>
                      {statusBadge(p.status)}
                      <span className="admin-subtitle">作者: {p.author_id}</span>
                    </div>
                    <div className="admin-actions">
                      <Button variant="danger" onClick={() => handleModerate(p.id, false)}>
                        <X size={14} />
                        拒绝
                      </Button>
                      <Button onClick={() => handleModerate(p.id, true)}>
                        <Check size={14} />
                        批准
                      </Button>
                    </div>
                  </div>
                  {p.description && <p className="page-subtitle" style={{ marginTop: '10px' }}>{p.description}</p>}
                  <pre className="persona-prompt-block">{p.system_prompt}</pre>
                  <input
                    type="text"
                    placeholder="审核备注（可选）"
                    value={notes[p.id] || ''}
                    onChange={(e) => setNotes((prev) => ({ ...prev, [p.id]: e.target.value }))}
                    className="persona-note-input"
                  />
                </div>
              ))}
            </div>
          )}
        </Card>
      )}

      {tab === 'pool' && (
        <Card className="animate-fade-in-up">
          {isLoading ? (
            <p className="admin-empty">加载中...</p>
          ) : pool.filter((p) => p.status === 'approved').length === 0 ? (
            <p className="admin-empty">公共池暂无已通过人设</p>
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column', gap: '16px' }}>
              {pool
                .filter((p) => p.status === 'approved')
                .map((p) => (
                  <div key={p.id} className="persona-review-item">
                    <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '12px' }}>
                      <div style={{ display: 'flex', alignItems: 'center', gap: '10px' }}>
                        <strong style={{ fontSize: '15px' }}>{p.name}</strong>
                        {statusBadge(p.status)}
                        {defaultId === p.id && <Badge variant="info">默认</Badge>}
                        <span className="admin-subtitle">作者: {p.author_id}</span>
                      </div>
                      <div className="admin-actions">
                        {defaultId === p.id ? (
                          <button className="icon-button" type="button" aria-label="取消默认" onClick={handleClearDefault}>
                            <Star size={14} fill="var(--accent)" />
                          </button>
                        ) : (
                          <button className="icon-button" type="button" aria-label="设为默认" onClick={() => handleSetDefault(p.id)}>
                            <Star size={14} />
                          </button>
                        )}
                        <button className="icon-button" type="button" aria-label="编辑" onClick={() => startEdit(p)}>
                          <Edit2 size={14} />
                        </button>
                        <Button variant="danger" onClick={() => handleModerate(p.id, false)}>
                          <X size={14} />
                          下架
                        </Button>
                        <button className="icon-button" type="button" aria-label="删除" onClick={() => handleDelete(p.id)}>
                          <Trash2 size={14} />
                        </button>
                      </div>
                    </div>
                    {p.description && <p className="page-subtitle" style={{ marginTop: '8px' }}>{p.description}</p>}
                    <pre className="persona-prompt-block">
                      {p.system_prompt.slice(0, 240)}
                      {p.system_prompt.length > 240 ? '...' : ''}
                    </pre>
                  </div>
                ))}
            </div>
          )}
        </Card>
      )}

      {tab === 'mine' && (
        <div className="grid grid-2">
          <Card className="animate-fade-in-up">
            <h3>{editingId ? '编辑人设' : '创建人设'}</h3>
            <form onSubmit={handleSave} style={{ display: 'flex', flexDirection: 'column', gap: '16px', marginTop: '20px' }}>
              <Input label="名称" placeholder="例如：代码审查官" value={name} onChange={(e) => setName(e.target.value)} />
              <Input label="描述" placeholder="简要说明这个人设的用途" value={description} onChange={(e) => setDescription(e.target.value)} />
              <label className="persona-label">
                系统提示词
                <textarea
                  className="persona-textarea"
                  rows={8}
                  value={systemPrompt}
                  onChange={(e) => setSystemPrompt(e.target.value)}
                />
              </label>
              {!editingId && (
                <label className="admin-switch" style={{ textTransform: 'none' }}>
                  <input type="checkbox" checked={isPublic} onChange={(e) => setIsPublic(e.target.checked)} />
                  <span>直接发布到公共池（管理员创建免审核）</span>
                </label>
              )}
              <div className="persona-actions">
                {editingId && (
                  <Button type="button" variant="secondary" onClick={resetForm}>
                    <RotateCcw size={14} />
                    取消
                  </Button>
                )}
                <Button type="submit" isLoading={isSaving}>
                  {editingId ? <Edit2 size={14} /> : <Plus size={14} />}
                  {editingId ? '保存' : '创建'}
                </Button>
              </div>
            </form>
          </Card>

          <Card className="animate-fade-in-up">
            <h3>我创建的人设（设默认 / 编辑 / 删除）</h3>
            {isLoading ? (
              <p className="page-subtitle">加载中...</p>
            ) : (
              <MinePersonaList
                pool={mine}
                defaultId={defaultId}
                onEdit={startEdit}
                onDelete={handleDelete}
                onSetDefault={handleSetDefault}
                onClearDefault={handleClearDefault}
              />
            )}
          </Card>
        </div>
      )}
    </div>
  );
}

function MinePersonaList({
  pool,
  defaultId,
  onEdit,
  onDelete,
  onSetDefault,
  onClearDefault,
}: {
  pool: Persona[];
  defaultId: string;
  onEdit: (p: Persona) => void;
  onDelete: (id: string) => void;
  onSetDefault: (id: string) => void;
  onClearDefault: () => void;
}) {
  if (pool.length === 0) {
    return <p className="page-subtitle">暂无人设</p>;
  }
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '12px', marginTop: '16px' }}>
      {pool.map((p) => (
        <div
          key={p.id}
          style={{
            padding: '14px',
            background: 'var(--bg-raised)',
            border: `1px solid ${defaultId === p.id ? 'var(--accent)' : 'var(--border)'}`,
            borderRadius: 'var(--radius-sm)',
          }}
        >
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '12px' }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: '10px' }}>
              <strong style={{ fontSize: '14px' }}>{p.name}</strong>
              {defaultId === p.id && <Badge variant="info">默认</Badge>}
              <span className="admin-subtitle">作者: {p.author_id}</span>
            </div>
            <div className="admin-actions">
              {defaultId === p.id ? (
                <button className="icon-button" type="button" aria-label="取消默认" onClick={onClearDefault}>
                  <Star size={14} fill="var(--accent)" />
                </button>
              ) : (
                <button className="icon-button" type="button" aria-label="设为默认" onClick={() => onSetDefault(p.id)}>
                  <Star size={14} />
                </button>
              )}
              <button className="icon-button" type="button" aria-label="编辑" onClick={() => onEdit(p)}>
                <Edit2 size={14} />
              </button>
              <button className="icon-button" type="button" aria-label="删除" onClick={() => onDelete(p.id)}>
                <Trash2 size={14} />
              </button>
            </div>
          </div>
        </div>
      ))}
    </div>
  );
}
