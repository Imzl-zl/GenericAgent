import { useEffect, useState, type FormEvent } from 'react';
import { RefreshCw } from 'lucide-react';
import { ApiClientError } from '../../api/client';
import {
  getAgentRuntimeSettings,
  getDocumentPoolSettings,
  getDocumentPoolStatus,
  getIMAggregationSettings,
  updateAgentRuntimeSettings,
  updateDocumentPoolSettings,
  updateIMAggregationSettings,
  type DocumentPoolStatus,
} from '../../api/settings';
import { Card } from '../../components/ui/Card';
import { Button } from '../../components/ui/Button';
import { Input } from '../../components/ui/Input';
import './AdminPages.css';

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof ApiClientError ? `${error.code}: ${error.message}` : fallback;
}

type DocumentPoolForm = {
  enabled: boolean;
  max_active: string;
  min_ready: string;
  job_idle_ttl_seconds: string;
  ready_idle_ttl_seconds: string;
  global_queue_limit: string;
  per_tenant_queue_limit: string;
  per_tenant_active_limit: string;
  job_timeout_seconds: string;
  command_timeout_seconds: string;
  version: number;
  deployment_max_active: number;
  reason: string;
};

const initialDocumentPool: DocumentPoolForm = {
  enabled: false,
  max_active: '1',
  min_ready: '0',
  job_idle_ttl_seconds: '600',
  ready_idle_ttl_seconds: '300',
  global_queue_limit: '100',
  per_tenant_queue_limit: '20',
  per_tenant_active_limit: '1',
  job_timeout_seconds: '3600',
  command_timeout_seconds: '300',
  version: 0,
  deployment_max_active: 0,
  reason: '',
};

const documentPoolNumberFields: Array<{ key: keyof DocumentPoolForm; label: string; min: number }> = [
  { key: 'max_active', label: '最大活动 Job 数', min: 0 },
  { key: 'min_ready', label: '最小 Ready 容量', min: 0 },
  { key: 'job_idle_ttl_seconds', label: 'Job 空闲 TTL（秒）', min: 1 },
  { key: 'ready_idle_ttl_seconds', label: 'Ready 空闲 TTL（秒）', min: 1 },
  { key: 'global_queue_limit', label: '全局队列上限', min: 1 },
  { key: 'per_tenant_queue_limit', label: '单租户队列上限', min: 1 },
  { key: 'per_tenant_active_limit', label: '单租户活动上限', min: 0 },
  { key: 'job_timeout_seconds', label: 'Job 超时（秒）', min: 1 },
  { key: 'command_timeout_seconds', label: '命令超时（秒）', min: 1 },
];

function queuedAgeLabel(status: DocumentPoolStatus): string {
  if (!status.oldest_queued_at) return '-';
  const seconds = Math.max(0, Math.floor((Date.parse(status.observed_at) - Date.parse(status.oldest_queued_at)) / 1000));
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`;
  return `${Math.floor(seconds / 3600)}h ${Math.floor((seconds % 3600) / 60)}m`;
}

export function SettingsPage() {
  const [windowMs, setWindowMs] = useState('2500');
  const [maxTurns, setMaxTurns] = useState('80');
  const [documentPool, setDocumentPool] = useState<DocumentPoolForm>(initialDocumentPool);
  const [documentStatus, setDocumentStatus] = useState<DocumentPoolStatus>();
  const [isLoading, setIsLoading] = useState(true);
  const [savingKey, setSavingKey] = useState<'aggregation' | 'agent' | 'document-pool' | ''>('');
  const [error, setError] = useState('');
  const [saved, setSaved] = useState('');

  useEffect(() => {
    let active = true;
    void Promise.all([getIMAggregationSettings(), getAgentRuntimeSettings(), getDocumentPoolSettings(), getDocumentPoolStatus()])
      .then(([aggregation, agent, pool, status]) => {
        if (!active) return;
        setWindowMs(String(aggregation.window_ms));
        setMaxTurns(String(agent.max_turns));
        setDocumentPool({
          enabled: pool.enabled,
          max_active: String(pool.max_active),
          min_ready: String(pool.min_ready),
          job_idle_ttl_seconds: String(pool.job_idle_ttl_seconds),
          ready_idle_ttl_seconds: String(pool.ready_idle_ttl_seconds),
          global_queue_limit: String(pool.global_queue_limit),
          per_tenant_queue_limit: String(pool.per_tenant_queue_limit),
          per_tenant_active_limit: String(pool.per_tenant_active_limit),
          job_timeout_seconds: String(pool.job_timeout_seconds),
          command_timeout_seconds: String(pool.command_timeout_seconds),
          version: pool.version,
          deployment_max_active: pool.deployment_max_active,
          reason: '',
        });
        setDocumentStatus(status);
      })
      .catch((loadError: unknown) => {
        if (active) setError(errorMessage(loadError, '加载失败'));
      })
      .finally(() => {
        if (active) setIsLoading(false);
      });
    const interval = window.setInterval(() => {
      void getDocumentPoolStatus()
        .then((status) => {
          if (active) setDocumentStatus(status);
        })
        .catch((loadError: unknown) => {
          if (active) setError(errorMessage(loadError, '刷新文档池状态失败'));
        });
    }, 10_000);
    return () => {
      active = false;
      window.clearInterval(interval);
    };
  }, []);

  const beginSave = (key: 'aggregation' | 'agent' | 'document-pool') => {
    setSavingKey(key);
    setError('');
    setSaved('');
  };

  const handleAggregationSave = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    beginSave('aggregation');
    try {
      const parsed = Number(windowMs);
      const response = await updateIMAggregationSettings({ window_ms: Number.isFinite(parsed) ? parsed : 0 });
      setWindowMs(String(response.window_ms));
      setSaved('微信聚合设置已保存');
    } catch (saveError) {
      setError(errorMessage(saveError, '保存失败'));
    } finally {
      setSavingKey('');
    }
  };

  const handleAgentSave = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    beginSave('agent');
    try {
      const parsed = Number(maxTurns);
      const response = await updateAgentRuntimeSettings({ max_turns: Number.isFinite(parsed) ? parsed : 0 });
      setMaxTurns(String(response.max_turns));
      setSaved('Agent 运行设置已保存');
    } catch (saveError) {
      setError(errorMessage(saveError, '保存失败'));
    } finally {
      setSavingKey('');
    }
  };

  const handleDocumentPoolSave = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    beginSave('document-pool');
    try {
      const numberValue = (key: keyof DocumentPoolForm) => Number(documentPool[key]);
      const response = await updateDocumentPoolSettings({
        enabled: documentPool.enabled,
        max_active: numberValue('max_active'),
        min_ready: numberValue('min_ready'),
        job_idle_ttl_seconds: numberValue('job_idle_ttl_seconds'),
        ready_idle_ttl_seconds: numberValue('ready_idle_ttl_seconds'),
        global_queue_limit: numberValue('global_queue_limit'),
        per_tenant_queue_limit: numberValue('per_tenant_queue_limit'),
        per_tenant_active_limit: numberValue('per_tenant_active_limit'),
        job_timeout_seconds: numberValue('job_timeout_seconds'),
        command_timeout_seconds: numberValue('command_timeout_seconds'),
        expected_version: documentPool.version,
        reason: documentPool.reason.trim(),
      });
      setDocumentPool((current) => ({
        ...current,
        version: response.version,
        deployment_max_active: response.deployment_max_active,
        reason: '',
      }));
      setSaved(response.apply_status === 'pending_retry'
        ? '文档池设置已保存；运行时应用失败，后台正在自动重试'
        : '文档池设置已保存并应用到运行时');
    } catch (saveError) {
      setError(errorMessage(saveError, '保存失败'));
    } finally {
      setSavingKey('');
    }
  };

  const refreshDocumentStatus = async () => {
    try {
      setDocumentStatus(await getDocumentPoolStatus());
      setError('');
    } catch (loadError) {
      setError(errorMessage(loadError, '刷新文档池状态失败'));
    }
  };

  const disabled = isLoading || savingKey !== '';

  return (
    <div className="admin-page">
      <header className="admin-header animate-fade-in-up">
        <div><h1>策略设置</h1><p className="admin-subtitle">任务执行、文档池与微信入站策略</p></div>
      </header>
      {error && <div className="provider-error" role="alert">{error}</div>}
      {saved && <div className="provider-success" role="status">{saved}</div>}

      <div className="admin-grid admin-grid-2">
        <Card className="animate-fade-in-up animate-delay-1">
          <h3>Agent 任务预算</h3>
          <form className="provider-form" onSubmit={handleAgentSave}>
            <Input label="最大执行轮次" type="number" min={10} max={500} step={10} value={maxTurns} disabled={disabled} onChange={(event) => setMaxTurns(event.target.value)} />
            <div className="settings-group" style={{ marginTop: '8px' }}><div className="settings-row"><div className="settings-row-info"><span className="settings-row-title">生效范围</span><span className="settings-row-desc">保存后对后续任务生效；正在运行的任务继续使用启动时的预算。</span></div></div></div>
            <div className="provider-form-full provider-actions"><Button isLoading={savingKey === 'agent'} disabled={disabled}>保存配置</Button></div>
          </form>
        </Card>

        <Card className="animate-fade-in-up animate-delay-2">
          <h3>微信入站聚合</h3>
          <form className="provider-form" onSubmit={handleAggregationSave}>
            <Input label="入站聚合窗口（毫秒）" type="number" min={0} max={5000} step={100} value={windowMs} disabled={disabled} onChange={(event) => setWindowMs(event.target.value)} />
            <div className="settings-group" style={{ marginTop: '8px' }}><div className="settings-row"><div className="settings-row-info"><span className="settings-row-title">说明</span><span className="settings-row-desc">仅对微信 IM 普通消息生效。0 表示关闭，建议范围 1500~2500ms。</span></div></div></div>
            <div className="provider-form-full provider-actions"><Button isLoading={savingKey === 'aggregation'} disabled={disabled}>保存配置</Button></div>
          </form>
        </Card>

        <Card className="document-pool-card animate-fade-in-up animate-delay-2">
          <div className="document-pool-heading">
            <h3>文档处理池</h3>
            <button className="icon-button" type="button" title="刷新运行状态" onClick={() => void refreshDocumentStatus()} disabled={isLoading}>
              <RefreshCw size={16} />
            </button>
          </div>
          <div className="document-pool-status" aria-live="polite">
            <div><span>排队 Job</span><strong>{documentStatus?.jobs_queued ?? '-'}</strong></div>
            <div><span>活动 Job</span><strong>{documentStatus ? documentStatus.jobs_starting + documentStatus.jobs_running : '-'}</strong></div>
            <div><span>Ready 实例</span><strong>{documentStatus?.instances_ready ?? '-'}</strong></div>
            <div><span>执行中命令</span><strong>{documentStatus?.commands_executing ?? '-'}</strong></div>
          </div>
          <div className="document-pool-status-detail">
            <span>创建中 {documentStatus?.instances_creating ?? '-'}</span>
            <span>销毁中 {documentStatus?.instances_destroying ?? '-'}</span>
            <span>Lost {documentStatus?.instances_lost ?? '-'}</span>
            <span>待执行命令 {documentStatus?.commands_pending ?? '-'}</span>
            <span>最老排队 {documentStatus ? queuedAgeLabel(documentStatus) : '-'}</span>
            <span>观测于 {documentStatus ? new Date(documentStatus.observed_at).toLocaleTimeString() : '-'}</span>
          </div>
          <form className="provider-form" onSubmit={handleDocumentPoolSave}>
            <label className="provider-form-full">
              <input type="checkbox" checked={documentPool.enabled} disabled={disabled} onChange={(event) => setDocumentPool((current) => ({ ...current, enabled: event.target.checked }))} /> 启用文档处理池
            </label>
            {documentPoolNumberFields.map((field) => (
              <Input
                key={field.key}
                label={field.label}
                type="number"
                min={field.min}
                max={field.key === 'max_active' && documentPool.deployment_max_active > 0 ? documentPool.deployment_max_active : undefined}
                step={1}
                value={String(documentPool[field.key])}
                disabled={disabled}
                onChange={(event) => setDocumentPool((current) => ({ ...current, [field.key]: event.target.value }))}
              />
            ))}
            <Input label="变更原因" value={documentPool.reason} disabled={disabled} required onChange={(event) => setDocumentPool((current) => ({ ...current, reason: event.target.value }))} />
            <div className="provider-form-full settings-group">
              <div className="settings-row"><div className="settings-row-info"><span className="settings-row-title">版本与部署上限</span><span className="settings-row-desc">当前版本 {documentPool.version}；max_active 部署硬上限 {documentPool.deployment_max_active}。保存采用版本 CAS，冲突时请刷新。</span></div></div>
              <div className="settings-row"><div className="settings-row-info"><span className="settings-row-title">生效语义</span><span className="settings-row-desc">完整配置原子保存后应用到运行时；失败时后台按数据库版本自动对账重试。新 Job 使用新配置，运行中 Job 不会被终止。</span></div></div>
            </div>
            <div className="provider-form-full provider-actions"><Button isLoading={savingKey === 'document-pool'} disabled={disabled}>保存文档池配置</Button></div>
          </form>
        </Card>
      </div>
    </div>
  );
}
