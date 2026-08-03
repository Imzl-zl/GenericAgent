import { useEffect, useState, type FormEvent } from 'react';
import { Check, Link2, LoaderCircle, RefreshCw, Search } from 'lucide-react';
import { ApiClientError } from '../../api/client';
import {
  bindSophub,
  getSophubBinding,
  searchSophub,
  type SophubBindingStatus,
  type SophubSearchItem,
} from '../../api/sops';
import { Badge } from '../../components/ui/Badge';
import { Button } from '../../components/ui/Button';
import { Card } from '../../components/ui/Card';
import { Input } from '../../components/ui/Input';
import './AdminPages.css';

const errorText = (error: unknown, fallback: string) =>
  error instanceof ApiClientError ? `${error.code}: ${error.message}` : fallback;

export function SOPAdminPage() {
  const [binding, setBinding] = useState<SophubBindingStatus>();
  const [apiKey, setApiKey] = useState('');
  const [query, setQuery] = useState('');
  const [results, setResults] = useState<SophubSearchItem[]>([]);
  const [isLoading, setIsLoading] = useState(true);
  const [isSaving, setIsSaving] = useState(false);
  const [isSearching, setIsSearching] = useState(false);
  const [error, setError] = useState('');
  const [success, setSuccess] = useState('');

  const refresh = async () => {
    setIsLoading(true);
    try {
      setBinding(await getSophubBinding());
      setError('');
    } catch (loadError) {
      setError(errorText(loadError, '加载 Sophub 连接状态失败'));
    } finally {
      setIsLoading(false);
    }
  };

  useEffect(() => {
    let active = true;
    void getSophubBinding()
      .then((nextBinding) => {
        if (active) setBinding(nextBinding);
      })
      .catch((loadError: unknown) => {
        if (active) setError(errorText(loadError, '加载 Sophub 连接状态失败'));
      })
      .finally(() => {
        if (active) setIsLoading(false);
      });
    return () => {
      active = false;
    };
  }, []);

  const notify = (message: string) => {
    setSuccess(message);
    setError('');
  };

  const bind = async (event: FormEvent) => {
    event.preventDefault();
    if (!apiKey.trim()) return;
    setIsSaving(true);
    setError('');
    try {
      setBinding(await bindSophub(apiKey.trim()));
      setApiKey('');
      notify('Sophub 连接已验证并保存');
    } catch (bindError) {
      setError(errorText(bindError, '连接 Sophub 失败'));
    } finally {
      setIsSaving(false);
    }
  };

  const search = async (event: FormEvent) => {
    event.preventDefault();
    setIsSearching(true);
    try {
      const reply = await searchSophub(query.trim());
      setResults(reply.items);
      setError('');
    } catch (searchError) {
      setError(errorText(searchError, '搜索 Sophub 失败'));
    } finally {
      setIsSearching(false);
    }
  };

  return (
    <div className="admin-page sop-admin-page">
      <header className="admin-header animate-fade-in-up">
        <div>
          <h1>Sophub 管理</h1>
          <p className="admin-subtitle">绑定 Sophub 平台账号，Worker 可在工作区直接安装公开 SOP</p>
        </div>
        <Button variant="secondary" onClick={() => void refresh()} disabled={isLoading}>
          <RefreshCw size={15} />刷新
        </Button>
      </header>

      {error && <div className="admin-error" role="alert">{error}</div>}
      {success && <div className="admin-success" role="status">{success}</div>}

      <div className="sop-connection-layout">
        <Card className="animate-fade-in-up">
          <div className="provider-panel-heading"><h3>连接状态</h3>{binding?.configured ? <Badge variant="success">已连接</Badge> : <Badge variant="default">未连接</Badge>}</div>
          {isLoading ? <p className="admin-empty">加载中...</p> : binding?.configured ? (
            <dl className="sop-detail-list">
              <div><dt>账户</dt><dd>{binding.display_name || binding.agent_uid}</dd></div>
              <div><dt>身份</dt><dd>{binding.author_type}</dd></div>
              <div><dt>Agent UID</dt><dd>{binding.agent_uid}</dd></div>
              <div><dt>最近验证</dt><dd>{binding.verified_at ? new Date(binding.verified_at).toLocaleString() : '-'}</dd></div>
            </dl>
          ) : <p className="admin-empty">尚未配置 Sophub API Key</p>}
        </Card>

        <Card className="animate-fade-in-up animate-delay-1">
          <div className="provider-panel-heading"><h3>{binding?.configured ? '更新凭据' : '绑定 Sophub'}</h3><Link2 size={17} /></div>
          <form className="sop-bind-form" onSubmit={bind}>
            <Input label="Sophub API Key" type="password" autoComplete="new-password" required value={apiKey} onChange={(event) => setApiKey(event.target.value)} />
            <Button type="submit" isLoading={isSaving} disabled={!apiKey.trim()}><Check size={15} />验证并保存</Button>
          </form>
        </Card>
      </div>

      <Card className="animate-fade-in-up animate-delay-2">
        <form className="sop-search" onSubmit={search}>
          <Input label="搜索公开 SOP" value={query} onChange={(event) => setQuery(event.target.value)} placeholder="标题或关键词" />
          <Button type="submit" variant="secondary" disabled={!binding?.configured || isSearching}>
            {isSearching ? <LoaderCircle className="sop-spin" size={15} /> : <Search size={15} />}搜索
          </Button>
        </form>
        {results.length > 0 && (
          <div className="sop-search-results">
            {results.map((item) => (
              <div className="sop-search-row" key={item.id}>
                <div><strong>{item.title}</strong><span>{item.preview || '无摘要'} · {item.file_type || item.package_type}</span></div>
              </div>
            ))}
          </div>
        )}
        {!isSearching && query !== '' && results.length === 0 && (
          <p className="admin-empty">未找到公开 SOP</p>
        )}
      </Card>
    </div>
  );
}
