import { Card } from '../../components/ui/Card';
import './AdminPages.css';

export function AdminDashboardPage() {
  return (
    <div className="admin-page">
      <header className="admin-header animate-fade-in-up">
        <div>
          <h1>运营控制台</h1>
          <p className="admin-subtitle">平台状态与待处理事项</p>
        </div>
      </header>

      <div className="admin-grid admin-grid-4">
        <Card className="animate-fade-in-up animate-delay-1">
          <p className="admin-metric-label">待审批用户</p>
          <p className="admin-metric-value">3</p>
        </Card>
        <Card className="animate-fade-in-up animate-delay-2">
          <p className="admin-metric-label">已批准用户</p>
          <p className="admin-metric-value accent">12</p>
        </Card>
        <Card className="animate-fade-in-up animate-delay-3">
          <p className="admin-metric-label">运行中任务</p>
          <p className="admin-metric-value cyan">2</p>
        </Card>
        <Card className="animate-fade-in-up animate-delay-4">
          <p className="admin-metric-label">活跃 Worker</p>
          <p className="admin-metric-value">4</p>
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
