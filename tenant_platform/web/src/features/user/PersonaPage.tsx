import { useEffect, useState } from 'react';
import { Card } from '../../components/ui/Card';
import { Button } from '../../components/ui/Button';
import { Input } from '../../components/ui/Input';
import { Badge } from '../../components/ui/Badge';
import { Edit2, Plus, RotateCcw, Send, Star, Trash2 } from 'lucide-react';
import {
  listPersonas,
  createPersona,
  updatePersona,
  deletePersona,
  submitPersona,
  setDefaultPersona,
  clearDefaultPersona,
} from '../../api/personas';
import { ApiClientError } from '../../api/client';
import type { Persona } from '../../api/types';
import './UserPages.css';

const DEFAULT_PERSONA_PROMPT = [
  '你是一位严谨的工程助手，擅长：',
  '- 阅读和分析技术文档',
  '- 在隔离环境中运行 Shell/Python',
  '- 给出可验证、可追溯的回答',
  '',
  '回答问题时优先引用工作区文件内容，不确定时明确说明。',
].join('\n');

const statusBadge = (status: string, isPublic: boolean) => {
  if (status === 'approved' && isPublic) return <Badge variant="success">公开</Badge>;
  if (status === 'approved') return <Badge variant="success">私有</Badge>;
  if (status === 'pending') return <Badge variant="warning">审核中</Badge>;
  if (status === 'rejected') return <Badge variant="danger">已拒绝</Badge>;
  return <Badge variant="default">私有</Badge>;
};

function isMine(p: Persona) {
  return p.status !== 'approved' || !p.is_public;
}

export function PersonaPage() {
  const [personas, setPersonas] = useState<Persona[]>([]);
  const [defaultId, setDefaultId] = useState<string>(() => localStorage.getItem('ga_default_persona_id') || '');
  const [error, setError] = useState('');
  const [success, setSuccess] = useState('');
  const [isLoading, setIsLoading] = useState(true);

  const [editingId, setEditingId] = useState<string | null>(null);
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [systemPrompt, setSystemPrompt] = useState(DEFAULT_PERSONA_PROMPT);
  const [isPublic, setIsPublic] = useState(false);
  const [isSaving, setIsSaving] = useState(false);

  const loadPersonas = async () => {
    setIsLoading(true);
    setError('');
    try {
      const list = await listPersonas();
      setPersonas(list);
    } catch (err) {
      setError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '加载失败');
    } finally {
      setIsLoading(false);
    }
  };

  useEffect(() => {
    let active = true;
    void listPersonas()
      .then((list) => {
        if (active) setPersonas(list);
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

  const resetForm = () => {
    setEditingId(null);
    setName('');
    setDescription('');
    setSystemPrompt(DEFAULT_PERSONA_PROMPT);
    setIsPublic(false);
  };

  const startEdit = (p: Persona) => {
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
    setIsSaving(true);

    if (!name.trim() || !systemPrompt.trim()) {
      setError('名称和系统提示词必填');
      setIsSaving(false);
      return;
    }

    try {
      if (editingId) {
        await updatePersona(editingId, name.trim(), description.trim(), systemPrompt.trim());
      } else {
        await createPersona(name.trim(), description.trim(), systemPrompt.trim(), isPublic);
      }
      setSuccess(editingId ? '更新成功' : '创建成功');
      resetForm();
      await loadPersonas();
    } catch (err) {
      setError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '保存失败');
    } finally {
      setIsSaving(false);
    }
  };

  const handleDelete = async (id: string) => {
    if (!window.confirm('确定删除这个人设？')) return;
    setError('');
    try {
      await deletePersona(id);
      if (defaultId === id) {
        setDefaultId('');
        localStorage.removeItem('ga_default_persona_id');
      }
      await loadPersonas();
    } catch (err) {
      setError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '删除失败');
    }
  };

  const handleSubmit = async (id: string) => {
    setError('');
    try {
      await submitPersona(id);
      await loadPersonas();
    } catch (err) {
      setError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '提交审核失败');
    }
  };

  const handleSetDefault = async (id: string) => {
    setError('');
    try {
      await setDefaultPersona(id);
      setDefaultId(id);
      localStorage.setItem('ga_default_persona_id', id);
      setSuccess('已设为默认');
    } catch (err) {
      setError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '设置失败');
    }
  };

  const handleClearDefault = async () => {
    setError('');
    try {
      await clearDefaultPersona();
      setDefaultId('');
      localStorage.removeItem('ga_default_persona_id');
      setSuccess('已取消默认');
    } catch (err) {
      setError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '取消失败');
    }
  };

  return (
    <div className="page">
      <header className="page-header animate-fade-in-up">
        <div>
          <h1>人设商店</h1>
          <p className="page-subtitle">创建、选择或提交自己的人设到公共商店</p>
        </div>
      </header>

      <div className="grid grid-2">
        <Card className="animate-fade-in-up animate-delay-1">
          <h3>{editingId ? '编辑人设' : '创建人设'}</h3>
          <form onSubmit={handleSave} style={{ display: 'flex', flexDirection: 'column', gap: '16px', marginTop: '20px' }}>
            <Input
              label="名称"
              placeholder="例如：代码审查官"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
            <Input
              label="描述"
              placeholder="简要说明这个人设的用途"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
            />
            <label className="persona-label">
              系统提示词
              <textarea
                className="persona-textarea"
                rows={8}
                value={systemPrompt}
                onChange={(e) => setSystemPrompt(e.target.value)}
              />
            </label>
            <label className="admin-switch" style={{ textTransform: 'none' }}>
              <input
                type="checkbox"
                checked={isPublic}
                onChange={(e) => setIsPublic(e.target.checked)}
                disabled={!!editingId}
              />
              <span>创建后提交到公共商店（需管理员审核）</span>
            </label>
            {error && <span className="input-error">{error}</span>}
            {success && <span className="input-error" style={{ color: 'var(--accent)' }}>{success}</span>}
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

        <Card className="animate-fade-in-up animate-delay-2">
          <h3>我的人设 / 公共商店</h3>
          {isLoading ? (
            <p className="page-subtitle">加载中...</p>
          ) : personas.length === 0 ? (
            <p className="page-subtitle">暂无人设</p>
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column', gap: '16px', marginTop: '16px' }}>
              {personas.map((p) => (
                <div
                  key={p.id}
                  style={{
                    padding: '16px',
                    background: 'var(--bg-raised)',
                    border: `1px solid ${defaultId === p.id ? 'var(--accent)' : 'var(--border)'}`,
                    borderRadius: 'var(--radius-sm)',
                  }}
                >
                  <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '12px' }}>
                    <div style={{ display: 'flex', alignItems: 'center', gap: '10px' }}>
                      <strong style={{ fontSize: '15px' }}>{p.name}</strong>
                      {statusBadge(p.status, p.is_public)}
                      {defaultId === p.id && <Badge variant="info">默认</Badge>}
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
                      {isMine(p) && p.status === 'private' && (
                        <button className="icon-button" type="button" aria-label="编辑" onClick={() => startEdit(p)}>
                          <Edit2 size={14} />
                        </button>
                      )}
                      {isMine(p) && p.status === 'private' && (
                        <button className="icon-button" type="button" aria-label="提交审核" onClick={() => handleSubmit(p.id)}>
                          <Send size={14} />
                        </button>
                      )}
                      {isMine(p) && (
                        <button className="icon-button" type="button" aria-label="删除" onClick={() => handleDelete(p.id)}>
                          <Trash2 size={14} />
                        </button>
                      )}
                    </div>
                  </div>
                  {p.description && (
                    <p className="page-subtitle" style={{ marginTop: '8px' }}>{p.description}</p>
                  )}
                  <pre style={{ marginTop: '10px', fontSize: '12px', color: 'var(--text-muted)', whiteSpace: 'pre-wrap' }}>
                    {p.system_prompt.slice(0, 160)}{p.system_prompt.length > 160 ? '...' : ''}
                  </pre>
                </div>
              ))}
            </div>
          )}
        </Card>
      </div>
    </div>
  );
}
