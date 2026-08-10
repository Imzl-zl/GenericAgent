export interface ApiError {
  code: string;
  message: string;
  trace_id: string;
}

export interface User {
  user_id: number;
  username: string;
  status: 'pending' | 'approved' | 'blocked';
  created_at: string;
  approved_at?: string;
}

export interface RegisterResponse extends User {
  token: string;
}

/** GET /v1/users/me 响应：当前用户实时状态 */
export interface MeResponse {
  user_id: number;
  username: string;
  status: 'pending' | 'approved' | 'blocked';
}

export interface InviteCode {
  code: string;
  state: 'active' | 'used' | 'revoked' | 'expired';
  created_by: number;
  used_by?: number;
  used_at?: string;
  expires_at: string;
  created_at: string;
}

export interface WechatQRCode {
  qrcode_token: string;
  qrcode_url: string;
  status: 'wait' | 'scaned' | 'scaned_but_redirect' | 'expired' | 'confirmed';
  expires_at: string;
}

export interface WechatQRCodeStatus {
  qrcode_token: string;
  status: 'wait' | 'scaned' | 'scaned_but_redirect' | 'expired' | 'confirmed';
  expires_at: string;
  bound: boolean;
  bot?: Bot;
}

export interface Bot {
  bot_id: number;
  bot_uuid: string;
  channel_type: string;
  ilink_bot_id: string;
  channel_account_id?: string;
  baseurl?: string;
  owner_id: number;
  state: string;
  created_at: string;
}

// 渠道绑定(IM_CHANNEL_BINDING §4): GET/PUT/DELETE /v1/me/im-bindings。
export type ChannelType = 'wechat' | 'feishu' | 'dingtalk' | 'qq';

export interface ChannelBinding {
  channel_type: ChannelType;
  state: 'active' | 'disabled' | 'expired' | 'revoked';
  bound_at: string;
  updated_at?: string;
  meta: {
    ilink_bot_id?: string;
    channel_account_id?: string;
    app_id?: string;
  };
}

export interface SaveChannelBindingRequest {
  app_id: string;
  app_secret: string;
}

export interface Persona {
  id: string;
  author_id: number;
  name: string;
  description: string;
  system_prompt: string;
  is_public: boolean;
  status: 'private' | 'pending' | 'approved' | 'rejected';
  admin_note?: string;
  created_at: string;
  updated_at: string;
}

export type LLMProviderType = 'native_oai' | 'native_claude';
export type ProviderAuthMode = 'auto' | 'bearer' | 'x_api_key';

export interface GASessionConfig {
  thinking_type?: 'adaptive' | 'enabled' | 'disabled';
  thinking_budget_tokens?: number;
  reasoning_effort?: 'none' | 'minimal' | 'low' | 'medium' | 'high' | 'xhigh' | 'max';
  temperature?: number;
  max_tokens?: number;
  context_win?: number;
  trim_keep_prefix?: number;
  max_retries?: number;
  read_timeout?: number;
  stream?: boolean;
  api_mode?: 'chat_completions' | 'responses';
  fake_cc_system_prompt?: boolean;
  user_agent?: string;
  service_tier?: 'auto' | 'default' | 'priority' | 'flex';
  omit_thinking?: boolean;
  extra_sys_prompt?: string;
}

export interface ProviderTransportConfig {
  auth_mode: ProviderAuthMode;
  proxy_url?: string;
  tls_verify?: boolean;
  connect_timeout_seconds?: number;
  response_header_timeout_seconds?: number;
}

export interface MCPServer {
  mcp_server_id: number;
  server_key: string;
  name: string;
  /** transport=http 时为第三方地址; transport=stdio 时为空(gateway 路由由平台合成) */
  url: string | null;
  transport: 'http' | 'stdio';
  /** stdio 专用: 镜像预装工具集白名单绝对路径 */
  command: string | null;
  args: string[] | null;
  isolation: 'shared' | 'workspace';
  max_instances: number;
  timeout_seconds: number;
  enabled: boolean;
  revision: number;
  created_at: string;
  updated_at: string;
}

export interface LLMProvider {
  provider_id: number;
  name: string;
  provider_type: LLMProviderType;
  base_url: string;
  model: string;
  session_config: GASessionConfig;
  transport_config: ProviderTransportConfig;
  revision: number;
  is_default: boolean;
  state: 'active' | 'disabled';
  created_at: string;
  updated_at: string;
}
