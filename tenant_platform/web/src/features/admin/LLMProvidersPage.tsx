import { useEffect, useState } from 'react';
import { Card } from '../../components/ui/Card';
import { Button } from '../../components/ui/Button';
import { Input } from '../../components/ui/Input';
import { Collapsible } from '../../components/ui/Collapsible';
import { Trash2, Star } from 'lucide-react';
import {
  listProviders,
  createProvider,
  deleteProvider,
  setDefaultProvider,
} from '../../api/providers';
import { ApiClientError } from '../../api/client';
import type { LLMProvider, LLMProviderConfig } from '../../api/types';
import './AdminPages.css';

export function LLMProvidersPage() {
  const [providers, setProviders] = useState<LLMProvider[]>([]);
  const [form, setForm] = useState({
    name: '',
    provider_type: 'native_oai' as 'native_oai' | 'native_claude',
    base_url: '',
    model: '',
    api_key: '',
    config: {
      // ── 推理 / 思考 ──
      thinking_type: 'adaptive' as 'adaptive' | 'enabled' | 'disabled' | undefined,
      thinking_budget_tokens: undefined as number | undefined,
      reasoning_effort: undefined as 'none' | 'minimal' | 'low' | 'medium' | 'high' | 'xhigh' | undefined,

      // ── 采样 ──
      max_tokens: undefined as number | undefined,
      temperature: undefined as number | undefined,
      top_p: undefined as number | undefined,

      // ── 容量 / 超时 ──
      context_win: undefined as number | undefined,
      max_retries: undefined as number | undefined,
      connect_timeout: undefined as number | undefined,
      read_timeout: undefined as number | undefined,
      timeout: undefined as number | undefined,

      // ── 传输 ──
      stream: true as boolean | undefined,
      api_mode: undefined as 'chat_completions' | 'responses' | undefined,

      // ── Claude 专属 ──
      fake_cc_system_prompt: undefined as boolean | undefined,
      user_agent: undefined as string | undefined,

      // ── 网络 ──
      proxy: undefined as string | undefined,
    } as LLMProviderConfig,
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
      setForm({
        name: '',
        provider_type: 'native_oai',
        base_url: '',
        model: '',
        api_key: '',
        config: {
          // ── 推理 / 思考 ──
          thinking_type: 'adaptive',
          thinking_budget_tokens: undefined,
          reasoning_effort: undefined,

          // ── 采样 ──
          max_tokens: undefined,
          temperature: undefined,
          top_p: undefined,

          // ── 容量 / 超时 ──
          context_win: undefined,
          max_retries: undefined,
          connect_timeout: undefined,
          read_timeout: undefined,
          timeout: undefined,

          // ── 传输 ──
          stream: true,
          api_mode: undefined,

          // ── Claude 专属 ──
          fake_cc_system_prompt: undefined,
          user_agent: undefined,

          // ── 网络 ──
          proxy: undefined,
        },
      });
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
            <div>
              <label className="input-label">类型</label>
              <select
                className="input-field"
                value={form.provider_type}
                onChange={(e) => setForm({ ...form, provider_type: e.target.value as 'native_oai' | 'native_claude' })}
                style={{ width: '100%', padding: '8px 12px' }}
              >
                <option value="native_oai">OpenAI (native_oai)</option>
                <option value="native_claude">Anthropic Claude (native_claude)</option>
              </select>
            </div>
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

            {/* GA Core 配置项 */}
            <div className="provider-form-full" style={{ borderTop: '1px solid var(--border)', paddingTop: '16px', marginTop: '8px' }}>
              <h4 style={{ marginBottom: '12px', fontSize: '14px', fontWeight: 500 }}>高级配置（可选）</h4>
            </div>

            <div className="provider-form-full">
              <Collapsible title="🧠 推理与思考" defaultOpen={false}>
                <div>
                  <label className="input-label">Thinking Type</label>
                  <select
                    className="input-field"
                    value={form.config.thinking_type || 'adaptive'}
                    onChange={(e) => setForm({ ...form, config: { ...form.config, thinking_type: e.target.value as any } })}
                    style={{ width: '100%', padding: '8px 12px' }}
                  >
                    <option value="adaptive">Adaptive（自适应）</option>
                    <option value="enabled">Enabled（启用）</option>
                    <option value="disabled">Disabled（禁用）</option>
                  </select>
                </div>

                {form.config.thinking_type === 'enabled' && (
                  <Input
                    label="Thinking Budget Tokens"
                    type="number"
                    placeholder="例如 10000"
                    value={form.config.thinking_budget_tokens || ''}
                    onChange={(e) => setForm({ ...form, config: { ...form.config, thinking_budget_tokens: e.target.value ? parseInt(e.target.value) : undefined } })}
                  />
                )}

                <div>
                  <label className="input-label">Reasoning Effort</label>
                  <select
                    className="input-field"
                    value={form.config.reasoning_effort || ''}
                    onChange={(e) => setForm({ ...form, config: { ...form.config, reasoning_effort: e.target.value as any || undefined } })}
                    style={{ width: '100%', padding: '8px 12px' }}
                  >
                    <option value="">默认</option>
                    <option value="none">None</option>
                    <option value="minimal">Minimal</option>
                    <option value="low">Low</option>
                    <option value="medium">Medium</option>
                    <option value="high">High</option>
                    <option value="xhigh">XHigh</option>
                  </select>
                </div>
              </Collapsible>
            </div>

            <Input
              label="Max Tokens"
              type="number"
              placeholder="例如 8192"
              value={form.config.max_tokens || ''}
              onChange={(e) => setForm({ ...form, config: { ...form.config, max_tokens: e.target.value ? parseInt(e.target.value) : undefined } })}
            />

            <Input
              label="Temperature"
              type="number"
              step="0.01"
              placeholder="0.0 - 2.0"
              value={form.config.temperature || ''}
              onChange={(e) => setForm({ ...form, config: { ...form.config, temperature: e.target.value ? parseFloat(e.target.value) : undefined } })}
            />

            <Input
              label="Top P"
              type="number"
              step="0.01"
              placeholder="0.0 - 1.0"
              value={form.config.top_p || ''}
              onChange={(e) => setForm({ ...form, config: { ...form.config, top_p: e.target.value ? parseFloat(e.target.value) : undefined } })}
            />

            <Input
              label="Max Retries"
              type="number"
              placeholder="例如 3"
              value={form.config.max_retries || ''}
              onChange={(e) => setForm({ ...form, config: { ...form.config, max_retries: e.target.value ? parseInt(e.target.value) : undefined } })}
            />

            <Input
              label="Timeout (秒)"
              type="number"
              placeholder="例如 60"
              value={form.config.timeout || ''}
              onChange={(e) => setForm({ ...form, config: { ...form.config, timeout: e.target.value ? parseInt(e.target.value) : undefined } })}
            />
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
