import { useEffect, useState, type FormEvent } from 'react';
import { Pencil, Power, PowerOff, Save, Trash2, X } from 'lucide-react';
import { ApiClientError } from '../../api/client';
import {
  createMCPServer,
  deleteMCPServer,
  listMCPServers,
  setMCPServerEnabled,
  updateMCPServer,
  type MCPServerWriteInput,
} from '../../api/mcpServers';
import type { MCPServer } from '../../api/types';
import { Button } from '../../components/ui/Button';
import { Card } from '../../components/ui/Card';
import { Input } from '../../components/ui/Input';
import './AdminPages.css';

type MCPFormValue = MCPServerWriteInput & {
  /** args 的逗号分隔编辑态 */
  args_text: string;
};

function initialForm(server?: MCPServer): MCPFormValue {
  return {
    server_key: server?.server_key ?? '',
    name: server?.name ?? '',
    transport: server?.transport ?? 'http',
    url: server?.url ?? '',
    command: server?.command ?? '',
    args: server?.args ?? [],
    args_text: server?.args?.join(', ') ?? '',
    max_instances: server?.max_instances ?? 1,
    timeout_seconds: server?.timeout_seconds ?? 30,
  };
}

function errorMessage(error: unknown, fallback: string): string {
  return error instanceof ApiClientError ? `${error.code}: ${error.message}` : fallback;
}

function fetchMCPServers(): Promise<MCPServer[]> {
  return listMCPServers();
}

export function MCPServersPage() {
  const [servers, setServers] = useState<MCPServer[]>([]);
  const [editing, setEditing] = useState<MCPServer>();
  const [form, setForm] = useState<MCPFormValue>(() => initialForm());
  const [error, setError] = useState('');
  const [isLoading, setIsLoading] = useState(true);
  const [isSaving, setIsSaving] = useState(false);

  const refresh = async () => {
    setIsLoading(true);
    try {
      setServers(await fetchMCPServers());
      setError('');
    } catch (loadError) {
      setError(errorMessage(loadError, '加载 MCP 配置失败'));
    } finally {
      setIsLoading(false);
    }
  };

  useEffect(() => {
    let active = true;
    void fetchMCPServers()
      .then((list) => {
        if (active) {
          setServers(list);
          setError('');
        }
      })
      .catch((loadError: unknown) => {
        if (active) setError(errorMessage(loadError, '加载 MCP 配置失败'));
      })
      .finally(() => {
        if (active) setIsLoading(false);
      });
    return () => {
      active = false;
    };
  }, []);

  const startEditing = (server: MCPServer) => {
    setEditing(server);
    setForm(initialForm(server));
    setError('');
  };

  const resetForm = () => {
    setEditing(undefined);
    setForm(initialForm());
  };

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setError('');
    const input: MCPServerWriteInput = {
      server_key: form.server_key,
      name: form.name,
      transport: form.transport,
      timeout_seconds: form.timeout_seconds,
      max_instances: form.max_instances,
      isolation: 'shared',
    };
    if (form.transport === 'http') {
      input.url = form.url;
    } else {
      input.command = form.command;
      input.args = form.args_text
        .split(',')
        .map((part) => part.trim())
        .filter(Boolean);
    }
    setIsSaving(true);
    try {
      if (editing) await updateMCPServer(editing.mcp_server_id, input);
      else await createMCPServer(input);
      resetForm();
      await refresh();
    } catch (saveError) {
      setError(errorMessage(saveError, '保存 MCP 配置失败'));
    } finally {
      setIsSaving(false);
    }
  };

  const changeState = async (server: MCPServer) => {
    const enabled = !server.enabled;
    if (enabled && !window.confirm(`启用 “${server.name}” 后，所有租户都能使用其工具。继续？`)) return;
    try {
      await setMCPServerEnabled(server.mcp_server_id, enabled);
      await refresh();
    } catch (stateError) {
      setError(errorMessage(stateError, enabled ? '启用失败' : '停用失败'));
    }
  };

  const remove = async (server: MCPServer) => {
    if (!window.confirm(`删除 MCP Server “${server.name}”？`)) return;
    try {
      await deleteMCPServer(server.mcp_server_id);
      if (editing?.mcp_server_id === server.mcp_server_id) resetForm();
      await refresh();
    } catch (deleteError) {
      setError(errorMessage(deleteError, '删除失败'));
    }
  };

  return (
    <div className="admin-page provider-page">
      <header className="admin-header animate-fade-in-up">
        <div>
          <h1>MCP Servers</h1>
          <p className="admin-subtitle">管理员启用后，工具对所有租户共享；配置变更在下一任务生效</p>
        </div>
      </header>

      {error && <div className="provider-error" role="alert">{error}</div>}

      <div className="provider-layout">
        <Card className="provider-editor animate-fade-in-up animate-delay-1">
          <div className="provider-panel-heading">
            <h3>{editing ? '编辑 MCP Server' : '新增 MCP Server'}</h3>
            {editing && <span>REV {editing.revision}</span>}
          </div>
          <form className="provider-form" onSubmit={submit}>
            <Input label="Server Key" required pattern="[A-Za-z0-9_]{1,32}" placeholder="exa" value={form.server_key} onChange={(event) => setForm({ ...form, server_key: event.target.value })} />
            <Input label="名称" required placeholder="Exa" value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} />
            <label className="input-wrapper provider-form-full">
              <span className="input-label">接入方式</span>
              <select
                className="input-field"
                value={form.transport}
                onChange={(event) => setForm({ ...form, transport: event.target.value as 'http' | 'stdio' })}
              >
                <option value="http">http（远程 URL）</option>
                <option value="stdio">stdio（镜像预装工具，经 mcp-gateway 托管）</option>
              </select>
            </label>
            {form.transport === 'http' ? (
              <Input className="provider-form-full" label="MCP URL" required type="url" placeholder="https://mcp.exa.ai/mcp" value={form.url ?? ''} onChange={(event) => setForm({ ...form, url: event.target.value })} />
            ) : (
              <>
                <Input className="provider-form-full" label="命令（/opt/mcp-tools/ 白名单绝对路径）" required placeholder="/opt/mcp-tools/mcp-pandoc" value={form.command ?? ''} onChange={(event) => setForm({ ...form, command: event.target.value })} />
                <Input className="provider-form-full" label="参数（逗号分隔，可空）" placeholder="--stdio" value={form.args_text} onChange={(event) => setForm({ ...form, args_text: event.target.value })} />
                <Input label="进程数上限（1-16）" type="number" min={1} max={16} value={form.max_instances ?? 1} onChange={(event) => setForm({ ...form, max_instances: Number(event.target.value) })} />
              </>
            )}
            <Input label="超时（秒）" required type="number" min={1} max={300} value={form.timeout_seconds} onChange={(event) => setForm({ ...form, timeout_seconds: Number(event.target.value) })} />
            <p className="admin-subtitle provider-form-full">stdio 工具由 mcp-gateway 托管：无网络、无凭据、tmpfs 工作目录；工具集随镜像预装。</p>
            <div className="provider-actions provider-form-full">
              {editing && <Button type="button" variant="ghost" onClick={resetForm}><X size={15} />取消</Button>}
              <Button type="submit" isLoading={isSaving}><Save size={15} />保存</Button>
            </div>
          </form>
        </Card>

        <Card className="provider-list-panel animate-fade-in-up animate-delay-2">
          <div className="provider-panel-heading"><h3>全局 MCP</h3><span>{servers.length} SERVERS</span></div>
          {isLoading ? <p className="admin-empty">加载中...</p> : servers.length === 0 ? <p className="admin-empty">暂无 MCP Server</p> : (
            <div className="provider-table-scroll">
              <table className="admin-table provider-table">
                <thead><tr><th>Server</th><th>接入</th><th>地址 / 命令</th><th aria-label="操作" /></tr></thead>
                <tbody>{servers.map((server) => (
                  <tr key={server.mcp_server_id}>
                    <td><strong>{server.name}</strong><small>{server.server_key} · REV {server.revision} · <span className={`provider-state ${server.enabled ? 'active' : 'disabled'}`}>{server.enabled ? 'ENABLED' : 'DISABLED'}</span></small></td>
                    <td><span className={`provider-state ${server.transport === 'stdio' ? 'active' : ''}`}>{server.transport}</span></td>
                    <td>{server.transport === 'stdio' ? `${server.command}${server.max_instances > 1 ? ` ×${server.max_instances}` : ''}` : server.url}</td>
                    <td><div className="admin-actions provider-row-actions">
                      <button className="icon-button" type="button" title={server.enabled ? '停用' : '启用'} onClick={() => void changeState(server)}>{server.enabled ? <PowerOff size={16} /> : <Power size={16} />}</button>
                      <button className="icon-button" type="button" title="编辑" onClick={() => startEditing(server)}><Pencil size={16} /></button>
                      <button className="icon-button danger" type="button" title="删除" onClick={() => void remove(server)}><Trash2 size={16} /></button>
                    </div></td>
                  </tr>
                ))}</tbody>
              </table>
            </div>
          )}
        </Card>
      </div>
    </div>
  );
}
