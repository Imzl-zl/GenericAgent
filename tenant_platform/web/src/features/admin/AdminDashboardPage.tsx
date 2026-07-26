import { useEffect, useState } from 'react';
import { Card } from '../../components/ui/Card';
import { getDashboardStats } from '../../api/stats';
import type { DashboardStats } from '../../api/stats';
import './AdminPages.css';

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
        </Card>
        <Card className="animate-fade-in-up animate-delay-2">
          <p className="admin-metric-label">已批准用户</p>
          <p className="admin-metric-value accent">
            {isLoading ? '...' : stats?.approved_users ?? 0}
          </p>
        </Card>
        <Card className="animate-fade-in-up animate-delay-3">
          <p className="admin-metric-label">运行中任务</p>
          <p className="admin-metric-value cyan">
            {isLoading ? '...' : stats?.running_tasks ?? 0}
          </p>
        </Card>
        <Card className="animate-fade-in-up animate-delay-4">
          <p className="admin-metric-label">活跃 Worker</p>
          <p className="admin-metric-value">
            {isLoading ? '...' : stats?.active_workers ?? 0}
          </p>
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
