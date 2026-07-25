import { Card } from '../../components/ui/Card';
import { Badge } from '../../components/ui/Badge';
import './UserPages.css';

export function StatusPage() {
  return (
    <div className="page">
      <header className="page-header animate-fade-in-up">
        <div>
          <h1>运行状态</h1>
          <p className="page-subtitle">当前 session 与任务队列</p>
        </div>
        <Badge variant="default">personal:42</Badge>
      </header>

      <div className="grid grid-4">
        <Card className="animate-fade-in-up animate-delay-1">
          <p className="metric-label">排队中</p>
          <p className="metric-value">0</p>
        </Card>
        <Card className="animate-fade-in-up animate-delay-2">
          <p className="metric-label">运行中</p>
          <p className="metric-value">0</p>
        </Card>
        <Card className="animate-fade-in-up animate-delay-3">
          <p className="metric-label">已完成</p>
          <p className="metric-value">0</p>
        </Card>
        <Card className="animate-fade-in-up animate-delay-4">
          <p className="metric-label">Worker</p>
          <p className="metric-value muted">IDLE</p>
        </Card>
      </div>

      <Card className="animate-fade-in-up animate-delay-5" style={{ marginTop: '24px' }}>
        <h3>最近任务</h3>
        <table className="data-table">
          <thead>
            <tr>
              <th>任务 ID</th>
              <th>状态</th>
              <th>来源</th>
              <th>创建时间</th>
            </tr>
          </thead>
          <tbody>
            <tr>
              <td colSpan={4} className="empty-cell">
                暂无任务
              </td>
            </tr>
          </tbody>
        </table>
      </Card>
    </div>
  );
}
