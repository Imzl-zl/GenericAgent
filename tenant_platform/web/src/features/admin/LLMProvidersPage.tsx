import { useEffect, useState } from 'react';
import { Pencil, Star, Trash2 } from 'lucide-react';
import { ApiClientError } from '../../api/client';
import {
  createProvider,
  deleteProvider,
  listProviders,
  setDefaultProvider,
  updateProvider,
  type UpdateProviderInput,
} from '../../api/providers';
import type { LLMProvider } from '../../api/types';
import { Card } from '../../components/ui/Card';
import { LLMProviderForm, type ProviderFormValue } from './LLMProviderForm';
import './AdminPages.css';

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof ApiClientError ? `${error.code}: ${error.message}` : fallback;
}

export function LLMProvidersPage() {
  const [providers, setProviders] = useState<LLMProvider[]>([]);
  const [editingProvider, setEditingProvider] = useState<LLMProvider>();
  const [error, setError] = useState('');
  const [isLoading, setIsLoading] = useState(true);

  const loadProviders = async () => {
    setIsLoading(true);
    try {
      setProviders(await listProviders());
    } catch (loadError) {
      setError(errorMessage(loadError, '加载失败'));
    } finally {
      setIsLoading(false);
    }
  };

  useEffect(() => {
    let active = true;
    void listProviders()
      .then((list) => {
        if (active) setProviders(list);
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

  const handleSave = async (value: ProviderFormValue): Promise<boolean> => {
    setError('');
    try {
      if (editingProvider) {
        const { api_key: apiKey, ...fields } = value;
        const input: UpdateProviderInput = apiKey.trim()
          ? { ...fields, api_key: apiKey }
          : fields;
        await updateProvider(editingProvider.provider_id, input);
      } else {
        await createProvider(value);
      }
      setEditingProvider(undefined);
      await loadProviders();
      return true;
    } catch (saveError) {
      setError(errorMessage(saveError, '保存失败'));
      return false;
    }
  };

  const handleDelete = async (provider: LLMProvider) => {
    if (!window.confirm(`删除 Provider “${provider.name}”？`)) {
      return;
    }
    setError('');
    try {
      await deleteProvider(provider.provider_id);
      if (editingProvider?.provider_id === provider.provider_id) {
        setEditingProvider(undefined);
      }
      await loadProviders();
    } catch (deleteError) {
      setError(errorMessage(deleteError, '删除失败'));
    }
  };

  const handleSetDefault = async (providerId: number) => {
    setError('');
    try {
      await setDefaultProvider(providerId);
      await loadProviders();
    } catch (defaultError) {
      setError(errorMessage(defaultError, '设置失败'));
    }
  };

  return (
    <div className="admin-page provider-page">
      <header className="admin-header animate-fade-in-up">
        <div>
          <h1>LLM Providers</h1>
          <p className="admin-subtitle">上游模型、GA 会话与网络传输</p>
        </div>
      </header>

      {error && <div className="provider-error" role="alert">{error}</div>}

      <div className="provider-layout">
        <Card className="provider-editor animate-fade-in-up animate-delay-1">
          <div className="provider-panel-heading">
            <h3>{editingProvider ? '编辑 Provider' : '新增 Provider'}</h3>
            {editingProvider && <span>REV {editingProvider.revision}</span>}
          </div>
          <LLMProviderForm
            key={editingProvider?.provider_id ?? 'create'}
            provider={editingProvider}
            onSave={handleSave}
            onCancel={() => setEditingProvider(undefined)}
          />
        </Card>

        <Card className="provider-list-panel animate-fade-in-up animate-delay-2">
          <div className="provider-panel-heading">
            <h3>已配置</h3>
            <span>{providers.length} PROVIDERS</span>
          </div>
          {isLoading ? (
            <p className="admin-empty">加载中...</p>
          ) : providers.length === 0 ? (
            <p className="admin-empty">暂无 Provider</p>
          ) : (
            <div className="provider-table-scroll">
              <table className="admin-table provider-table">
                <thead>
                  <tr>
                    <th>Provider</th>
                    <th>模型</th>
                    <th>默认</th>
                    <th aria-label="操作" />
                  </tr>
                </thead>
                <tbody>
                  {providers.map((provider) => (
                    <tr key={provider.provider_id}>
                      <td>
                        <strong>{provider.name}</strong>
                        <small>{provider.provider_type} · REV {provider.revision}</small>
                        <small className="provider-model-mobile">{provider.model}</small>
                      </td>
                      <td>{provider.model}</td>
                      <td>
                        {provider.is_default ? (
                          <span className="provider-default"><Star size={13} fill="currentColor" /> DEFAULT</span>
                        ) : (
                          <button
                            className="icon-button"
                            type="button"
                            title="设为默认"
                            aria-label={`将 ${provider.name} 设为默认`}
                            onClick={() => void handleSetDefault(provider.provider_id)}
                          >
                            <Star size={16} />
                          </button>
                        )}
                      </td>
                      <td>
                        <div className="admin-actions provider-row-actions">
                          <button
                            className="icon-button"
                            type="button"
                            title="编辑"
                            aria-label={`编辑 ${provider.name}`}
                            onClick={() => setEditingProvider(provider)}
                          >
                            <Pencil size={16} />
                          </button>
                          <button
                            className="icon-button danger"
                            type="button"
                            title="删除"
                            aria-label={`删除 ${provider.name}`}
                            onClick={() => void handleDelete(provider)}
                          >
                            <Trash2 size={16} />
                          </button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>
      </div>
    </div>
  );
}
