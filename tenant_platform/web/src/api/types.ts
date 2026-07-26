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

export interface InviteCode {
  code: string;
  state: 'active' | 'used' | 'revoked' | 'expired';
  created_by: number;
  used_by?: number;
  used_at?: string;
  expires_at: string;
  created_at: string;
}

export interface BindingCode {
  code: string;
  binding_id: number;
  user_id: number;
  state: string;
  expires_at: string;
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
  ilink_bot_id: string;
  ilink_user_id?: string;
  baseurl?: string;
  owner_id: number;
  state: string;
  created_at: string;
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

export interface LLMProvider {
  provider_id: number;
  name: string;
  provider_type: 'native_oai' | 'native_claude';
  base_url: string;
  model: string;
  config?: LLMProviderConfig;
  is_default: boolean;
  state: string;
  created_at: string;
  updated_at: string;
}

export interface LLMProviderConfig {
  // ── 推理 / 思考 ──
  thinking_type?: 'adaptive' | 'enabled' | 'disabled';
  thinking_budget_tokens?: number;
  reasoning_effort?: 'none' | 'minimal' | 'low' | 'medium' | 'high' | 'xhigh';

  // ── 采样 ──
  max_tokens?: number;
  temperature?: number;
  top_p?: number;

  // ── 容量 / 超时 ──
  context_win?: number;
  max_retries?: number;
  connect_timeout?: number;
  read_timeout?: number;
  timeout?: number;

  // ── 传输 ──
  stream?: boolean;
  api_mode?: 'chat_completions' | 'responses';

  // ── Claude 专属 ──
  fake_cc_system_prompt?: boolean;
  user_agent?: string;

  // ── 网络 ──
  proxy?: string;
}
