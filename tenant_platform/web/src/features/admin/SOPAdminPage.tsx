import { useEffect, useState, type FormEvent } from 'react';
import {
  Check,
  Download,
  Link2,
  LoaderCircle,
  Power,
  PowerOff,
  RefreshCw,
  Search,
  X,
} from 'lucide-react';
import { ApiClientError } from '../../api/client';
import {
  approveSOPCandidate,
  bindSophub,
  getSophubBinding,
  importSOPCandidate,
  listSOPCandidates,
  listSOPRegistry,
  loadSOPVersion,
  rejectSOPCandidate,
  searchSophub,
  unloadSOPEntry,
  type SOPCandidate,
  type SOPRegistryItem,
  type SophubBindingStatus,
  type SophubSearchItem,
} from '../../api/sops';
import { Badge } from '../../components/ui/Badge';
import { Button } from '../../components/ui/Button';
import { Card } from '../../components/ui/Card';
import { Input } from '../../components/ui/Input';
import './AdminPages.css';

type Tab = 'connection' | 'candidates' | 'installed';

const errorText = (error: unknown, fallback: string) =>
  error instanceof ApiClientError ? `${error.code}: ${error.message}` : fallback;

const shortDigest = (digest: string) => `${digest.slice(0, 12)}...${digest.slice(-8)}`;

const candidateBadge = (status: SOPCandidate['status']) => {
  if (status === 'approved') return <Badge variant="success">已安装</Badge>;
  if (status === 'rejected') return <Badge variant="danger">已拒绝</Badge>;
  return <Badge variant="warning">待审核</Badge>;
};

export function SOPAdminPage() {
  const [tab, setTab] = useState<Tab>('connection');
  const [binding, setBinding] = useState<SophubBindingStatus>();
  const [candidates, setCandidates] = useState<SOPCandidate[]>([]);
  const [registry, setRegistry] = useState<SOPRegistryItem[]>([]);
  const [apiKey, setApiKey] = useState('');
  const [query, setQuery] = useState('');
  const [results, setResults] = useState<SophubSearchItem[]>([]);
  const [isLoading, setIsLoading] = useState(true);
  const [isSaving, setIsSaving] = useState(false);
  const [busyId, setBusyId] = useState('');
  const [error, setError] = useState('');
  const [success, setSuccess] = useState('');

  const refresh = async () => {
    setIsLoading(true);
    try {
      const [nextBinding, nextCandidates, nextRegistry] = await Promise.all([
        getSophubBinding(),
        listSOPCandidates(),
        listSOPRegistry(),
      ]);
      setBinding(nextBinding);
      setCandidates(nextCandidates);
      setRegistry(nextRegistry);
      setError('');
    } catch (loadError) {
      setError(errorText(loadError, '加载 SOP 管理数据失败'));
    } finally {
      setIsLoading(false);
    }
  };

  useEffect(() => {
    let active = true;
    void Promise.all([getSophubBinding(), listSOPCandidates(), listSOPRegistry()])
      .then(([nextBinding, nextCandidates, nextRegistry]) => {
        if (!active) return;
        setBinding(nextBinding);
        setCandidates(nextCandidates);
        setRegistry(nextRegistry);
        setError('');
      })
      .catch((loadError: unknown) => {
        if (active) setError(errorText(loadError, '加载 SOP 管理数据失败'));
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
    setBusyId('search');
    try {
      const reply = await searchSophub(query.trim());
      setResults(reply.items);
      setError('');
    } catch (searchError) {
      setError(errorText(searchError, '搜索 Sophub 失败'));
    } finally {
      setBusyId('');
    }
  };

  const importCandidate = async (item: SophubSearchItem) => {
    setBusyId(item.id);
    try {
      await importSOPCandidate(item.id);
      notify(`“${item.title}”已进入审核队列`);
      setCandidates(await listSOPCandidates());
    } catch (importError) {
      setError(errorText(importError, '导入候选失败'));
    } finally {
      setBusyId('');
    }
  };

  const review = async (candidate: SOPCandidate, approved: boolean) => {
    let note = '';
    if (!approved) {
      const value = window.prompt('拒绝原因（可选）', candidate.review_note);
      if (value === null) return;
      note = value;
    }
    setBusyId(candidate.candidate_id);
    try {
      if (approved) await approveSOPCandidate(candidate.candidate_id);
      else await rejectSOPCandidate(candidate.candidate_id, note);
      notify(approved ? `“${candidate.title}”已安装` : `“${candidate.title}”已拒绝`);
      const [nextCandidates, nextRegistry] = await Promise.all([listSOPCandidates(), listSOPRegistry()]);
      setCandidates(nextCandidates);
      setRegistry(nextRegistry);
    } catch (reviewError) {
      setError(errorText(reviewError, approved ? '安装 SOP 失败' : '拒绝候选失败'));
    } finally {
      setBusyId('');
    }
  };

  const toggleLoaded = async (item: SOPRegistryItem) => {
    const action = item.loaded ? '卸载' : '加载';
    if (!window.confirm(`${action} “${item.title}” 版本 ${item.version}？`)) return;
    setBusyId(item.version_id);
    try {
      if (item.loaded) await unloadSOPEntry(item.entry_id);
      else await loadSOPVersion(item.version_id);
      notify(`“${item.title}”已${action}`);
      setRegistry(await listSOPRegistry());
    } catch (toggleError) {
      setError(errorText(toggleError, `${action} SOP 失败`));
    } finally {
      setBusyId('');
    }
  };

  const tabs: { key: Tab; label: string }[] = [
    { key: 'connection', label: 'Sophub 连接' },
    { key: 'candidates', label: `候选审核 (${candidates.filter((item) => item.status === 'pending').length})` },
    { key: 'installed', label: `已安装 (${registry.length})` },
  ];

  return (
    <div className="admin-page sop-admin-page">
      <header className="admin-header animate-fade-in-up">
        <div>
          <h1>SOP 管理</h1>
          <p className="admin-subtitle">从 Sophub 审核安装，并控制新任务自动加载的版本</p>
        </div>
        <Button variant="secondary" onClick={() => void refresh()} disabled={isLoading}>
          <RefreshCw size={15} />刷新
        </Button>
      </header>

      <div className="sop-tabs" role="tablist" aria-label="SOP 管理视图">
        {tabs.map((item) => (
          <button
            key={item.key}
            type="button"
            role="tab"
            aria-selected={tab === item.key}
            className={tab === item.key ? 'is-active' : ''}
            onClick={() => setTab(item.key)}
          >
            {item.label}
          </button>
        ))}
      </div>

      {error && <div className="admin-error" role="alert">{error}</div>}
      {success && <div className="admin-success" role="status">{success}</div>}

      {tab === 'connection' && (
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
      )}

      {tab === 'candidates' && (
        <div className="sop-panel-stack">
          <Card className="animate-fade-in-up">
            <form className="sop-search" onSubmit={search}>
              <Input label="搜索 Sophub" value={query} onChange={(event) => setQuery(event.target.value)} placeholder="标题或关键词" />
              <Button type="submit" variant="secondary" disabled={!binding?.configured || busyId === 'search'}>
                {busyId === 'search' ? <LoaderCircle className="sop-spin" size={15} /> : <Search size={15} />}搜索
              </Button>
            </form>
            {results.length > 0 && (
              <div className="sop-search-results">
                {results.map((item) => (
                  <div className="sop-search-row" key={item.id}>
                    <div><strong>{item.title}</strong><span>{item.preview || '无摘要'} · {item.file_type || item.package_type}</span></div>
                    <Button variant="ghost" onClick={() => void importCandidate(item)} disabled={busyId === item.id}><Download size={15} />导入候选</Button>
                  </div>
                ))}
              </div>
            )}
          </Card>

          <Card className="animate-fade-in-up animate-delay-1">
            <div className="provider-panel-heading"><h3>审核队列</h3><span>{candidates.length} CANDIDATES</span></div>
            {isLoading ? <p className="admin-empty">加载中...</p> : candidates.length === 0 ? <p className="admin-empty">暂无候选 SOP</p> : (
              <div className="sop-review-list">
                {candidates.map((candidate) => (
                  <article className="sop-review-row" key={candidate.candidate_id}>
                    <div className="sop-row-heading">
                      <div><strong>{candidate.title}</strong><span>{candidate.remote_sop_id} · {candidate.file_type}</span></div>
                      {candidateBadge(candidate.status)}
                    </div>
                    {candidate.description && <p>{candidate.description}</p>}
                    <details><summary>查看候选正文</summary><pre>{candidate.content}</pre></details>
                    {candidate.review_note && <p className="sop-review-note">审核备注：{candidate.review_note}</p>}
                    {candidate.status === 'pending' && (
                      <div className="admin-actions sop-review-actions">
                        <Button variant="danger" onClick={() => void review(candidate, false)} disabled={busyId === candidate.candidate_id}><X size={15} />拒绝</Button>
                        <Button onClick={() => void review(candidate, true)} disabled={busyId === candidate.candidate_id}><Check size={15} />安装</Button>
                      </div>
                    )}
                  </article>
                ))}
              </div>
            )}
          </Card>
        </div>
      )}

      {tab === 'installed' && (
        <Card className="animate-fade-in-up">
          <div className="provider-panel-heading"><h3>本地不可变版本</h3><span>{registry.filter((item) => item.loaded).length} LOADED</span></div>
          {isLoading ? <p className="admin-empty">加载中...</p> : registry.length === 0 ? <p className="admin-empty">尚未安装 SOP</p> : (
            <div className="provider-table-scroll">
              <table className="admin-table sop-registry-table">
                <thead><tr><th>SOP</th><th>Digest</th><th>状态</th><th aria-label="操作" /></tr></thead>
                <tbody>{registry.map((item) => (
                  <tr key={item.version_id}>
                    <td><strong>{item.title}</strong><small>VERSION {item.version} · {new Date(item.approved_at).toLocaleDateString()}</small><small className="sop-registry-mobile-meta">{item.loaded ? 'LOADED' : 'NOT LOADED'} · {shortDigest(item.digest)}</small></td>
                    <td><code title={item.digest}>{shortDigest(item.digest)}</code></td>
                    <td>{item.loaded ? <Badge variant="success">已加载</Badge> : <Badge variant="default">未加载</Badge>}</td>
                    <td><button className="icon-button" type="button" title={item.loaded ? '卸载' : '加载'} onClick={() => void toggleLoaded(item)} disabled={busyId === item.version_id}>{item.loaded ? <PowerOff size={16} /> : <Power size={16} />}</button></td>
                  </tr>
                ))}</tbody>
              </table>
            </div>
          )}
        </Card>
      )}
    </div>
  );
}
