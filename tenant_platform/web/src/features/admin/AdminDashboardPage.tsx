import { useEffect, useState } from 'react';
import { Card } from '../../components/ui/Card';
import { getDashboardStats } from '../../api/stats';
import type { DashboardStats } from '../../api/stats';
import './AdminPages.css';

function formatSeconds(value: number, disabledLabel = '禁用'): string {
  if (!value) return disabledLabel;
  if (value % 3600 === 0) return `${value / 3600}h`;
  if (value % 60 === 0) return `${value / 60}m`;
  return `${value}s`;
}

function formatLimit(value: number): string {
  return value > 0 ? String(value) : '未限制';
}

export function AdminDashboardPage() {
  const [stats, setStats] = useState<DashboardStats | null>(null);
  const [isLoading, setIsLoading] = useState(true);
  const [error, setError] = useState('');

  useEffect(() => {
    const loadStats = async () => {
      setIsLoading(true);
      setError('');
      try {
        const data = await getDashboardStats();
        setStats(data);
      } catch (err) {
        setError('加载统计数据失败');
        console.error('加载统计数据失败', err);
      } finally {
        setIsLoading(false);
      }
    };
    loadStats();
  }, []);

  return (
    <div className="admin-page">
      <header className="admin-header animate-fade-in-up">
        <div>
          <h1>运营控制台</h1>
          <p className="admin-subtitle">平台状态与待处理事项</p>
        </div>
      </header>

      {error && (
        <div style={{ color: 'var(--color-error)', marginBottom: '16px' }}>
          {error}
        </div>
      )}

      <div className="admin-grid admin-grid-4">
        <Card className="animate-fade-in-up animate-delay-1">
          <p className="admin-metric-label">待审批用户</p>
          <p className="admin-metric-value">
            {isLoading ? '...' : stats?.pending_users ?? 0}
          </p>
          <p className="admin-metric-desc">与「用户审批」页列表一致</p>
        </Card>
        <Card className="animate-fade-in-up animate-delay-2">
          <p className="admin-metric-label">已批准用户</p>
          <p className="admin-metric-value accent">
            {isLoading ? '...' : stats?.approved_users ?? 0}
          </p>
          <p className="admin-metric-desc">累计数，含系统自建账号（bootstrap、内置管理员）</p>
        </Card>
        <Card className="animate-fade-in-up animate-delay-3">
          <p className="admin-metric-label">运行中任务</p>
          <p className="admin-metric-value cyan">
            {isLoading ? '...' : stats?.running_tasks ?? 0}
          </p>
        </Card>
      </div>

      <div className="admin-grid admin-grid-2">
        <Card className="admin-section animate-fade-in-up animate-delay-5">
          <h3>任务运行阈值</h3>
          <p className="admin-subtitle">当前生效的任务超时、租约与 token 边界</p>
          <div className="settings-group" style={{ marginTop: '16px' }}>
            <div className="settings-row">
              <div className="settings-row-info">
                <span className="settings-row-title">整任务硬上限</span>
                <span className="settings-row-desc">超过后调度器会强制终止任务</span>
              </div>
              <span className="admin-chip-value">{isLoading ? '...' : formatSeconds(stats?.runtime_profile.max_task_wall_clock_seconds ?? 0, '未配置')}</span>
            </div>
            <div className="settings-row">
              <div className="settings-row-info">
                <span className="settings-row-title">Worker 单次软超时</span>
                <span className="settings-row-desc">单次调用过久时由 Worker 发起取消</span>
              </div>
              <span className="admin-chip-value">{isLoading ? '...' : formatSeconds(stats?.runtime_profile.task_timeout_seconds ?? 0)}</span>
            </div>
            <div className="settings-row">
              <div className="settings-row-info">
                <span className="settings-row-title">Idle 卡死检测</span>
                <span className="settings-row-desc">无 chunk / heartbeat 多久后判定疑似卡死</span>
              </div>
              <span className="admin-chip-value">{isLoading ? '...' : formatSeconds(stats?.runtime_profile.task_idle_timeout_seconds ?? 0)}</span>
            </div>
            <div className="settings-row">
              <div className="settings-row-info">
                <span className="settings-row-title">Worker 空闲回收</span>
                <span className="settings-row-desc">会话空闲多久后回收常驻 Worker</span>
              </div>
              <span className="admin-chip-value">{isLoading ? '...' : formatSeconds(stats?.runtime_profile.worker_idle_ttl_seconds ?? 0)}</span>
            </div>
            <div className="settings-row">
              <div className="settings-row-info">
                <span className="settings-row-title">Claim Lease</span>
                <span className="settings-row-desc">调度器续租周期依赖的任务 claim 期限</span>
              </div>
              <span className="admin-chip-value">{isLoading ? '...' : formatSeconds(stats?.runtime_profile.claim_lease_seconds ?? 0, '未配置')}</span>
            </div>
            <div className="settings-row">
              <div className="settings-row-info">
                <span className="settings-row-title">Capability Token TTL / Refresh Skew</span>
                <span className="settings-row-desc">硬总时长必须被 token 生命周期覆盖</span>
              </div>
              <span className="admin-chip-value">{isLoading ? '...' : `${formatSeconds(stats?.runtime_profile.token_ttl_seconds ?? 0, '未配置')} / ${formatSeconds(stats?.runtime_profile.token_refresh_skew_seconds ?? 0, '未配置')}`}</span>
            </div>
          </div>
        </Card>

        <Card className="admin-section animate-fade-in-up animate-delay-5">
          <h3>并发与排队配额</h3>
          <p className="admin-subtitle">当前生效的全局、租户与用户限额</p>
          <div className="settings-group" style={{ marginTop: '16px' }}>
            <div className="settings-row">
              <div className="settings-row-info">
                <span className="settings-row-title">全局运行中任务上限</span>
                <span className="settings-row-desc">starting / running 的全局总数阈值</span>
              </div>
              <span className="admin-chip-value">{isLoading ? '...' : formatLimit(stats?.runtime_profile.max_running_tasks ?? 0)}</span>
            </div>
            <div className="settings-row">
              <div className="settings-row-info">
                <span className="settings-row-title">单租户并发上限</span>
                <span className="settings-row-desc">单 requester 同时 starting / running 的上限</span>
              </div>
              <span className="admin-chip-value">{isLoading ? '...' : formatLimit(stats?.runtime_profile.per_requester_running_limit ?? 0)}</span>
            </div>
            <div className="settings-row">
              <div className="settings-row-info">
                <span className="settings-row-title">单用户排队上限</span>
                <span className="settings-row-desc">单 requester 的 queued 任务上限</span>
              </div>
              <span className="admin-chip-value">{isLoading ? '...' : formatLimit(stats?.runtime_profile.per_user_queue_limit ?? 0)}</span>
            </div>
          </div>
        </Card>
      </div>

      <Card className="admin-section animate-fade-in-up animate-delay-5">
        <h3>系统状态</h3>
        <p className="admin-subtitle">所有服务运行正常</p>
        <div className="settings-group" style={{ marginTop: '16px' }}>
          <div className="settings-row">
            <div className="settings-row-info">
              <span className="settings-row-title">Platform</span>
              <span className="settings-row-desc">Web / Auth / IM Gateway / Router</span>
            </div>
            <span className="admin-metric-value accent" style={{ fontSize: '14px' }}>OK</span>
          </div>
          <div className="settings-row">
            <div className="settings-row-info">
              <span className="settings-row-title">Worker Manager</span>
              <span className="settings-row-desc">rootless Podman 生命周期</span>
            </div>
            <span className="admin-metric-value accent" style={{ fontSize: '14px' }}>OK</span>
          </div>
          <div className="settings-row">
            <div className="settings-row-info">
              <span className="settings-row-title">LLM Proxy</span>
              <span className="settings-row-desc">上游 Key 代理与配额</span>
            </div>
            <span className="admin-metric-value accent" style={{ fontSize: '14px' }}>OK</span>
          </div>
          <div className="settings-row">
            <div className="settings-row-info">
              <span className="settings-row-title">PostgreSQL</span>
              <span className="settings-row-desc">任务事实来源</span>
            </div>
            <span className="admin-metric-value accent" style={{ fontSize: '14px' }}>OK</span>
          </div>
        </div>
      </Card>
    </div>
  );
}
