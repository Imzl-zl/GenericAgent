import { useEffect, useState, type FormEvent } from 'react';
import { ApiClientError } from '../../api/client';
import {
  getAgentRuntimeSettings,
  getIMAggregationSettings,
  updateAgentRuntimeSettings,
  updateIMAggregationSettings,
} from '../../api/settings';
import { Card } from '../../components/ui/Card';
import { Button } from '../../components/ui/Button';
import { Input } from '../../components/ui/Input';
import './AdminPages.css';

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof ApiClientError ? `${error.code}: ${error.message}` : fallback;
}

export function SettingsPage() {
  const [windowMs, setWindowMs] = useState('2500');
  const [maxTurns, setMaxTurns] = useState('80');
  const [isLoading, setIsLoading] = useState(true);
  const [savingKey, setSavingKey] = useState<'aggregation' | 'agent' | ''>('');
  const [error, setError] = useState('');
  const [saved, setSaved] = useState('');

  useEffect(() => {
    let active = true;
    void Promise.all([getIMAggregationSettings(), getAgentRuntimeSettings()])
      .then(([aggregation, agent]) => {
        if (active) {
          setWindowMs(String(aggregation.window_ms));
          setMaxTurns(String(agent.max_turns));
        }
      })
      .catch((loadError: unknown) => {
        if (active) {
          setError(errorMessage(loadError, '加载失败'));
        }
      })
      .finally(() => {
        if (active) {
          setIsLoading(false);
        }
      });
    return () => {
      active = false;
    };
  }, []);

  const handleAggregationSave = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setSavingKey('aggregation');
    setError('');
    setSaved('');
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
    setSavingKey('agent');
    setError('');
    setSaved('');
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

  return (
    <div className="admin-page">
      <header className="admin-header animate-fade-in-up">
        <div>
          <h1>策略设置</h1>
          <p className="admin-subtitle">任务执行与微信入站策略</p>
        </div>
      </header>

      {error && <div className="provider-error" role="alert">{error}</div>}
      {saved && <div className="provider-success" role="status">{saved}</div>}

      <div className="admin-grid admin-grid-2">
        <Card className="animate-fade-in-up animate-delay-1">
          <h3>Agent 任务预算</h3>
          <form className="provider-form" onSubmit={handleAgentSave}>
            <Input
              label="最大执行轮次"
              type="number"
              min={10}
              max={500}
              step={10}
              value={maxTurns}
              disabled={isLoading || savingKey !== ''}
              onChange={(event) => setMaxTurns(event.target.value)}
            />
            <div className="settings-group" style={{ marginTop: '8px' }}>
              <div className="settings-row">
                <div className="settings-row-info">
                  <span className="settings-row-title">生效范围</span>
                  <span className="settings-row-desc">
                    保存后对后续任务生效；正在运行的任务继续使用启动时的预算。
                  </span>
                </div>
              </div>
              <div className="settings-row">
                <div className="settings-row-info">
                  <span className="settings-row-title">失败策略</span>
                  <span className="settings-row-desc">
                    达到上限仍未完成时，任务会明确标记为失败并上报 MAX_TURNS_EXCEEDED。
                  </span>
                </div>
              </div>
            </div>
            <div className="provider-form-full provider-actions">
              <Button isLoading={savingKey === 'agent'} disabled={isLoading || savingKey !== ''}>保存配置</Button>
            </div>
          </form>
        </Card>

        <Card className="animate-fade-in-up animate-delay-2">
          <h3>微信入站聚合</h3>
          <form className="provider-form" onSubmit={handleAggregationSave}>
            <Input
              label="入站聚合窗口（毫秒）"
              type="number"
              min={0}
              max={5000}
              step={100}
              value={windowMs}
              disabled={isLoading || savingKey !== ''}
              onChange={(event) => setWindowMs(event.target.value)}
            />
            <div className="settings-group" style={{ marginTop: '8px' }}>
              <div className="settings-row">
                <div className="settings-row-info">
                  <span className="settings-row-title">说明</span>
                  <span className="settings-row-desc">
                    仅对微信 IM 普通消息生效。优先按 context_token 聚合；0 表示关闭。建议范围 1500~2500ms。
                  </span>
                </div>
              </div>
              <div className="settings-row">
                <div className="settings-row-info">
                  <span className="settings-row-title">旁路规则</span>
                  <span className="settings-row-desc">
                    /stop、/new 等命令消息不等待窗口，仍然立即处理。
                  </span>
                </div>
              </div>
            </div>
            <div className="provider-form-full provider-actions">
              <Button isLoading={savingKey === 'aggregation'} disabled={isLoading || savingKey !== ''}>保存配置</Button>
            </div>
          </form>
        </Card>
      </div>
    </div>
  );
}
