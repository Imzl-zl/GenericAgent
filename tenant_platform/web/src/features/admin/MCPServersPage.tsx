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
import './AdminPages.css';

// JSON 直接编辑(EPIC mcp-governance D4'): 配置 = mcp.json 风格 JSON。
// transport 支持两种接入方式(主流 mcp.json 兼容):
//   http  (默认): url + headers(平台侧持有, proxy 注入, 回显掩码);
//   stdio: command + args(Worker 沙箱内进程宿主, 如 serena)。
// headers 值回显掩码(前 4 字符 + ***); 编辑时保持掩码 = 后端保留原 key,
// 填写明文 = 更新; 新增键直接写明文。
interface ServerConfigJSON {
  server_key: string;
  name: string;
  url: string;
  headers?: Record<string, string>;
  timeout_seconds: number;
  transport?: 'http' | 'stdio';
  command?: string;
  args?: string[];
}

function serverToJSON(server?: MCPServer): string {
  const config: ServerConfigJSON = {
    server_key: server?.server_key ?? '',
    name: server?.name ?? '',
    url: server?.url ?? '',
    timeout_seconds: server?.timeout_seconds ?? 30,
  };
  if (server?.transport && server.transport !== 'http') {
    config.transport = server.transport;
  }
  if (server?.command) {
    config.command = server.command;
  }
  if (server?.args && server.args.length > 0) {
    config.args = server.args;
  }
  if (server?.headers && Object.keys(server.headers).length > 0) {
    config.headers = server.headers;
  }
  return JSON.stringify(config, null, 2);
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
  const [configText, setConfigText] = useState(() => serverToJSON());
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
    setConfigText(serverToJSON(server));
    setError('');
  };

  const resetForm = () => {
    setEditing(undefined);
    setConfigText(serverToJSON());
  };

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setError('');
    let config: ServerConfigJSON;
    try {
      config = JSON.parse(configText) as ServerConfigJSON;
    } catch (parseError) {
      setError(`JSON 解析失败: ${parseError instanceof Error ? parseError.message : String(parseError)}`);
      return;
    }
    if (!config.server_key || !config.name || !config.timeout_seconds) {
      setError('server_key / name / timeout_seconds 均为必填');
      return;
    }
    const transport = config.transport ?? 'http';
    if (transport === 'stdio') {
      if (!config.url && !config.command) {
        setError('stdio 传输必须填写 command(如 serena); url 需留空');
        return;
      }
      if (config.url) {
        setError('stdio 传输不支持 url(进程在 Worker 沙箱内拉起)');
        return;
      }
      if (config.headers && Object.keys(config.headers).length > 0) {
        setError('stdio 传输不支持 headers(无 HTTP 请求头)');
        return;
      }
    } else if (!config.url) {
      setError('http 传输必须填写 url');
      return;
    }
    const input: MCPServerWriteInput = {
      server_key: config.server_key,
      name: config.name,
      url: config.url,
      timeout_seconds: config.timeout_seconds,
      transport,
    };
    if (transport === 'stdio') {
      input.command = config.command;
      input.args = config.args ?? [];
    }
    if (config.headers && Object.keys(config.headers).length > 0) {
      input.headers = config.headers;
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
          <p className="admin-subtitle">mcp.json 风格 JSON 直接编辑；key 平台侧持有（proxy 注入，回显掩码）；配置变更下一任务生效</p>
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
            <label className="input-wrapper provider-form-full">
              <span className="input-label">配置 JSON</span>
              <textarea
                className="input-field mcp-json-editor"
                rows={14}
                spellCheck={false}
                value={configText}
                onChange={(event) => setConfigText(event.target.value)}
                placeholder={JSON.stringify({
                  server_key: 'exa',
                  name: 'Exa',
                  url: 'https://mcp.exa.ai/mcp',
                  headers: { 'x-api-key': 'exa-xxx' },
                  timeout_seconds: 30,
                }, null, 2)}
              />
            </label>
            <p className="admin-subtitle provider-form-full">
              示例：{'{ "headers": { "Authorization": "Bearer tvly-xxx" } }'} 或 {'{ "headers": { "x-api-key": "..." } }'}(两种鉴权头都支持)。
              编辑已有 key 时保持掩码值(如 "tvly***")不变更，写明文即更新。
            </p>
            <p className="admin-subtitle provider-form-full">
              stdio 示例(mcp.json 主流格式, Worker 沙箱内进程宿主):
              {'{ "transport": "stdio", "command": "serena", "args": ["start-mcp-server", "--context=agent", "--project-from-cwd"] }'}。
              http 为默认 transport, 可省略。stdio 调用不经过 Platform proxy, 不参与按调用配额计量。
            </p>
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
                <thead><tr><th>Server</th><th>地址</th><th aria-label="操作" /></tr></thead>
                <tbody>{servers.map((server) => (
                  <tr key={server.mcp_server_id}>
                    <td><strong>{server.name}</strong><small>{server.server_key} · REV {server.revision} · <span className={`provider-state ${server.enabled ? 'active' : 'disabled'}`}>{server.enabled ? 'ENABLED' : 'DISABLED'}</span></small></td>
                    <td>{server.transport === 'stdio' ? `${server.command ?? ''} ${(server.args ?? []).join(' ')}` : server.url}</td>
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
