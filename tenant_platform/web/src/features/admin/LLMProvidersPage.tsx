import { useEffect, useState } from 'react';
import { Card } from '../../components/ui/Card';
import { Button } from '../../components/ui/Button';
import { Input } from '../../components/ui/Input';
import { Trash2, Star } from 'lucide-react';
import {
  listProviders,
  createProvider,
  deleteProvider,
  setDefaultProvider,
} from '../../api/providers';
import { ApiClientError } from '../../api/client';
import type { LLMProvider } from '../../api/types';
import './AdminPages.css';

export function LLMProvidersPage() {
  const [providers, setProviders] = useState<LLMProvider[]>([]);
  const [form, setForm] = useState({
    name: '',
    provider_type: 'openai',
    base_url: '',
    model: '',
    api_key: '',
  });
  const [error, setError] = useState('');
  const [isLoading, setIsLoading] = useState(true);

  const loadProviders = async () => {
    setIsLoading(true);
    setError('');
    try {
      const list = await listProviders();
      setProviders(list);
    } catch (err) {
      setError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '加载失败');
    } finally {
      setIsLoading(false);
    }
  };

  useEffect(() => {
    loadProviders();
  }, []);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError('');
    try {
      await createProvider(form);
      setForm({ name: '', provider_type: 'openai', base_url: '', model: '', api_key: '' });
      await loadProviders();
    } catch (err) {
      setError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '保存失败');
    }
  };

  const handleDelete = async (id: number) => {
    try {
      await deleteProvider(id);
      await loadProviders();
    } catch (err) {
      setError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '删除失败');
    }
  };

  const handleSetDefault = async (id: number) => {
    try {
      await setDefaultProvider(id);
      await loadProviders();
    } catch (err) {
      setError(err instanceof ApiClientError ? `${err.code}: ${err.message}` : '设置失败');
    }
  };

  return (
    <div className="admin-page">
      <header className="admin-header animate-fade-in-up">
        <div>
          <h1>LLM 供应</h1>
          <p className="admin-subtitle">配置上游模型与默认 Provider</p>
        </div>
      </header>

      <div className="admin-grid admin-grid-2">
        <Card className="animate-fade-in-up animate-delay-1">
          <h3>新增 Provider</h3>
          {error && <span className="input-error" style={{ display: 'block', marginBottom: '12px' }}>{error}</span>}
          <form className="provider-form" onSubmit={handleSubmit}>
            <Input
              label="名称"
              placeholder="例如 OpenAI Default"
              value={form.name}
              onChange={(e) => setForm({ ...form, name: e.target.value })}
            />
            <Input
              label="类型"
              placeholder="openai | anthropic"
              value={form.provider_type}
              onChange={(e) => setForm({ ...form, provider_type: e.target.value })}
            />
            <Input
              label="Base URL"
              placeholder="https://api.openai.com/v1"
              value={form.base_url}
              onChange={(e) => setForm({ ...form, base_url: e.target.value })}
            />
            <Input
              label="模型"
              placeholder="gpt-4o"
              value={form.model}
              onChange={(e) => setForm({ ...form, model: e.target.value })}
            />
            <div className="provider-form-full">
              <Input
                label="API Key"
                type="password"
                placeholder="sk-..."
                value={form.api_key}
                onChange={(e) => setForm({ ...form, api_key: e.target.value })}
              />
            </div>
            <div className="provider-form-full provider-actions">
              <Button type="submit">保存</Button>
            </div>
          </form>
        </Card>

        <Card className="animate-fade-in-up animate-delay-2">
          <h3>已配置</h3>
          {isLoading ? (
            <p className="admin-empty">加载中...</p>
          ) : (
            <table className="admin-table">
              <thead>
                <tr>
                  <th>名称</th>
                  <th>模型</th>
                  <th>默认</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                {providers.map((provider) => (
                  <tr key={provider.provider_id}>
                    <td>{provider.name}</td>
                    <td>{provider.model}</td>
                    <td>
                      {provider.is_default ? (
                        <Star size={14} color="var(--accent)" fill="var(--accent)" />
                      ) : (
                        <button
                          className="icon-button"
                          type="button"
                          onClick={() => handleSetDefault(provider.provider_id)}
                          aria-label="设为默认"
                        >
                          <Star size={16} />
                        </button>
                      )}
                    </td>
                    <td>
                      <button
                        className="icon-button"
                        type="button"
                        onClick={() => handleDelete(provider.provider_id)}
                        aria-label="删除"
                      >
                        <Trash2 size={16} />
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Card>
      </div>
    </div>
  );
}
