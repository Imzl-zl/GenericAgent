import { Card } from '../../components/ui/Card';
import { Button } from '../../components/ui/Button';
import { Input } from '../../components/ui/Input';
import './AdminPages.css';

export function SettingsPage() {
  return (
    <div className="admin-page">
      <header className="admin-header animate-fade-in-up">
        <div>
          <h1>策略设置</h1>
          <p className="admin-subtitle">运行时并发与工具策略</p>
        </div>
      </header>

      <div className="admin-grid admin-grid-2">
        <Card className="animate-fade-in-up animate-delay-1">
          <h3>运行时并发</h3>
          <form className="provider-form" onSubmit={(e) => e.preventDefault()}>
            <Input label="MAX_RUNNING_TASKS" type="number" defaultValue="4" />
            <Input label="MAX_LLM_INFLIGHT" type="number" defaultValue="8" />
            <Input label="PER_TENANT_RUNNING_LIMIT" type="number" defaultValue="2" />
            <Input label="PER_TENANT_QUEUE_LIMIT" type="number" defaultValue="10" />
            <div className="provider-form-full provider-actions">
              <Button>保存配置</Button>
            </div>
          </form>
        </Card>

        <Card className="animate-fade-in-up animate-delay-2">
          <h3>当前策略版本</h3>
          <div className="settings-group" style={{ marginTop: '16px' }}>
            <div className="settings-row">
              <div className="settings-row-info">
                <span className="settings-row-title">Capability Version</span>
                <span className="settings-row-desc">foundation.v1</span>
              </div>
            </div>
            <div className="settings-row">
              <div className="settings-row-info">
                <span className="settings-row-title">Tool Policy</span>
                <span className="settings-row-desc">foundation.no-host-tools.v1</span>
              </div>
            </div>
            <div className="settings-row">
              <div className="settings-row-info">
                <span className="settings-row-title">Worker Runtime</span>
                <span className="settings-row-desc">loopback | podman</span>
              </div>
            </div>
          </div>
        </Card>
      </div>
    </div>
  );
}
