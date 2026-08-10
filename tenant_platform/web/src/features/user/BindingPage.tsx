import { useEffect, useState, useRef, useCallback } from 'react';
import { Card } from '../../components/ui/Card';
import { Button } from '../../components/ui/Button';
import { Badge } from '../../components/ui/Badge';
import { QrCode, RefreshCw, Shield, Smartphone, Clock, MessageSquare, Save, Unlink, Link2 } from 'lucide-react';
import { QRCodeSVG } from 'qrcode.react';
import {
  createWechatQRCode,
  getWechatQRCodeStatus,
  createAdminWechatQRCode,
  getAdminWechatQRCodeStatus,
  listBindings,
  saveBinding,
  unbindChannel,
  listAdminBindings,
  saveAdminBinding,
  unbindAdminChannel,
} from '../../api/bots';
import { ApiClientError } from '../../api/client';
import { useAuth } from '../../contexts/AuthContext';
import type { ChannelBinding, ChannelType, WechatQRCode, WechatQRCodeStatus } from '../../api/types';
import './UserPages.css';

const POLL_INTERVAL_MS = 3000;

// 凭据表单渠道(飞书/钉钉/QQ)的展示元信息(IM_CHANNEL_BINDING §2 矩阵)。
interface ChannelMeta {
  key: ChannelType;
  label: string;
  appIdLabel: string;
  appIdHint: string;
  help: string[];
}

const CREDENTIAL_CHANNELS: ChannelMeta[] = [
  {
    key: 'feishu',
    label: '飞书',
    appIdLabel: 'App ID',
    appIdHint: '企业自建应用 App ID(cli_ 开头)',
    help: [
      '在飞书开放平台创建企业自建应用',
      '开通「机器人」能力并发布应用',
      '权限: 申请 im:message.p2p_msg(私聊)与 group_at_msg(仅@群消息, 不申请收全部的敏感权限 group_msg)',
      '将 App ID / App Secret 填入本页保存',
    ],
  },
  {
    key: 'dingtalk',
    label: '钉钉',
    appIdLabel: 'App Key',
    appIdHint: '开放平台机器人 AppKey',
    help: [
      '在钉钉开放平台创建企业内部应用',
      '添加「机器人」并发布',
      '群聊中只有 @ 机器人的消息可被接收(平台硬规则)',
      '将 App Key / App Secret 填入本页保存',
    ],
  },
  {
    key: 'qq',
    label: 'QQ',
    appIdLabel: 'App ID',
    appIdHint: 'QQ 开放平台机器人 AppID',
    help: [
      '在 QQ 开放平台创建机器人并完成审核',
      '沙箱环境需添加测试成员',
      '群聊只有 @ 机器人的消息可被接收(平台硬规则)',
      '将 App ID / App Secret 填入本页保存',
    ],
  },
];

function bindingErrorText(err: unknown, fallback: string) {
  if (err instanceof ApiClientError) {
    if (err.code === 'FEATURE_DISABLED') {
      return '微信绑定功能未启用（未配置 iLink，请联系管理员）';
    }
    return `${err.code}: ${err.message}`;
  }
  return fallback;
}

const statusLabel = (s?: string) => {
  switch (s) {
    case 'wait': return '等待扫码';
    case 'scaned': return '已扫码，等待确认';
    case 'scaned_but_redirect': return '扫码成功，正在连接';
    case 'expired': return '二维码已过期';
    case 'confirmed': return '绑定成功';
    default: return '未知';
  }
};

const formatExpiry = (s: string) => {
  const d = new Date(s);
  return d.toLocaleString('zh-CN', { hour12: false });
};

export function BindingPage() {
  const { state } = useAuth();
  const isAdmin = state?.isAdmin ?? false;

  const [bindings, setBindings] = useState<ChannelBinding[]>([]);
  const [loaded, setLoaded] = useState(false);

  // 微信扫码状态(原流程保留)
  const [qr, setQr] = useState<WechatQRCode | null>(null);
  const [wxStatus, setWxStatus] = useState<WechatQRCodeStatus | null>(null);
  const [isGenerating, setIsGenerating] = useState(false);
  const [error, setError] = useState('');
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);

  // 凭据表单状态(每渠道一份)
  const [forms, setForms] = useState<Record<string, { app_id: string; app_secret: string }>>({});
  const [saving, setSaving] = useState<Record<string, boolean>>({});
  const [unbinding, setUnbinding] = useState<Record<string, boolean>>({});
  const [formErrors, setFormErrors] = useState<Record<string, string>>({});

  const refreshBindings = useCallback(async () => {
    try {
      const list = isAdmin ? await listAdminBindings() : await listBindings();
      setBindings(list);
    } catch (err) {
      setError(bindingErrorText(err, '获取渠道绑定状态失败'));
    } finally {
      setLoaded(true);
    }
  }, [isAdmin]);

  useEffect(() => {
    void refreshBindings();
    return () => {
      clearTimeout(pollRef.current ?? undefined);
    };
  }, [refreshBindings]);

  const bindingOf = (t: ChannelType): ChannelBinding | undefined =>
    bindings.find((b) => b.channel_type === t);

  // ---- 微信扫码流程(原逻辑) ----
  const startPolling = (qrcodeToken: string) => {
    stopPolling();
    const tick = async () => {
      try {
        const s = isAdmin
          ? await getAdminWechatQRCodeStatus(qrcodeToken)
          : await getWechatQRCodeStatus(qrcodeToken);
        setWxStatus(s);
        if (s.status === 'confirmed' || s.status === 'expired') {
          if (s.status === 'confirmed') {
            void refreshBindings();
          }
          return;
        }
        if (s.bound) {
          void refreshBindings();
        }
      } catch (err) {
        setError(bindingErrorText(err, '轮询状态失败'));
      }
      if (pollRef.current) {
        pollRef.current = setTimeout(tick, POLL_INTERVAL_MS);
      }
    };
    pollRef.current = setTimeout(tick, 0);
  };

  const stopPolling = () => {
    if (pollRef.current) {
      clearTimeout(pollRef.current);
      pollRef.current = null;
    }
  };

  const handleGenerate = async () => {
    setError('');
    setWxStatus(null);
    stopPolling();
    setIsGenerating(true);
    try {
      const code = isAdmin ? await createAdminWechatQRCode() : await createWechatQRCode();
      setQr(code);
      startPolling(code.qrcode_token);
    } catch (err) {
      setError(bindingErrorText(err, '获取微信二维码失败'));
    } finally {
      setIsGenerating(false);
    }
  };

  // ---- 凭据渠道表单 ----
  const handleSave = async (meta: ChannelMeta) => {
    const f = forms[meta.key] ?? { app_id: '', app_secret: '' };
    if (!f.app_id.trim() || !f.app_secret.trim()) {
      setFormErrors((p) => ({ ...p, [meta.key]: 'App ID 与 App Secret 均为必填' }));
      return;
    }
    setError('');
    setFormErrors((p) => ({ ...p, [meta.key]: '' }));
    setSaving((p) => ({ ...p, [meta.key]: true }));
    try {
      if (isAdmin) {
        await saveAdminBinding(meta.key, { app_id: f.app_id.trim(), app_secret: f.app_secret.trim() });
      } else {
        await saveBinding(meta.key, { app_id: f.app_id.trim(), app_secret: f.app_secret.trim() });
      }
      setForms((p) => ({ ...p, [meta.key]: { app_id: '', app_secret: '' } }));
      await refreshBindings();
    } catch (err) {
      setFormErrors((p) => ({ ...p, [meta.key]: bindingErrorText(err, '保存失败') }));
    } finally {
      setSaving((p) => ({ ...p, [meta.key]: false }));
    }
  };

  const handleUnbind = async (meta: ChannelMeta) => {
    setError('');
    setUnbinding((p) => ({ ...p, [meta.key]: true }));
    try {
      if (isAdmin) {
        await unbindAdminChannel(meta.key);
      } else {
        await unbindChannel(meta.key);
      }
      await refreshBindings();
    } catch (err) {
      setFormErrors((p) => ({ ...p, [meta.key]: bindingErrorText(err, '解绑失败') }));
    } finally {
      setUnbinding((p) => ({ ...p, [meta.key]: false }));
    }
  };

  const wechat = bindingOf('wechat');
  const wechatBound = !!wechat && wechat.state === 'active' && !qr;

  const renderWechatCard = () => (
    <Card className="animate-fade-in-up animate-delay-1">
      <h3 style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
        <QrCode size={18} />
        微信
        {wechatBound && <Badge variant="success">已绑定</Badge>}
      </h3>
      <p className="page-subtitle">个人自用渠道：使用微信官方 iLink 扫码登录完成绑定</p>

      {wechatBound && !qr ? (
        <div style={{ marginTop: '16px', display: 'grid', gap: '12px' }}>
          <div>
            <div className="metric-label">iLink Bot ID</div>
            <code style={{ fontSize: '14px' }}>{wechat.meta.ilink_bot_id ?? '-'}</code>
          </div>
          {wechat.meta.channel_account_id && (
            <div>
              <div className="metric-label">iLink User ID</div>
              <code style={{ fontSize: '14px', color: 'var(--text-muted)' }}>{wechat.meta.channel_account_id}</code>
            </div>
          )}
          <div>
            <Button type="button" variant="secondary" onClick={handleGenerate} isLoading={isGenerating}>
              <RefreshCw size={16} />
              重新绑定
            </Button>
          </div>
        </div>
      ) : (
        <>
          {qr ? (
            <div style={{ marginTop: '20px', display: 'flex', flexDirection: 'column', gap: '20px', alignItems: 'center' }}>
              <div
                style={{
                  background: '#fff',
                  padding: '16px',
                  borderRadius: '12px',
                  boxShadow: '0 0 24px rgba(0, 229, 255, 0.12)',
                  maxWidth: '240px',
                }}
              >
                {qr.qrcode_url ? (
                  <QRCodeSVG value={qr.qrcode_url} size={208} level="M" includeMargin />
                ) : (
                  <div style={{ width: '208px', height: '208px', display: 'flex', alignItems: 'center', justifyContent: 'center', color: '#000' }}>
                    二维码加载中
                  </div>
                )}
              </div>
              <div style={{ display: 'flex', alignItems: 'center', gap: '8px', color: 'var(--text-muted)' }}>
                <Clock size={14} />
                <span>有效期至 {formatExpiry(qr.expires_at)}</span>
              </div>
              {wxStatus && (
                <div
                  style={{
                    width: '100%',
                    padding: '12px 16px',
                    borderRadius: '8px',
                    border: '1px solid var(--border)',
                    display: 'flex',
                    alignItems: 'center',
                    justifyContent: 'space-between',
                  }}
                >
                  <span>状态</span>
                  <Badge variant={wxStatus.status === 'confirmed' ? 'success' : wxStatus.status === 'expired' ? 'danger' : 'default'}>
                    {statusLabel(wxStatus.status)}
                  </Badge>
                </div>
              )}
              {(wxStatus?.status === 'scaned' || wxStatus?.status === 'scaned_but_redirect') && (
                <p style={{ margin: 0, fontSize: '13px', color: 'var(--text-muted)' }}>
                  已扫码！请在微信中确认授权，并保持本页面打开等待绑定完成
                </p>
              )}
              <Button type="button" variant="secondary" onClick={handleGenerate} isLoading={isGenerating}>
                <RefreshCw size={16} />
                重新获取
              </Button>
            </div>
          ) : (
            <Button type="button" onClick={handleGenerate} isLoading={isGenerating} style={{ marginTop: '20px', width: '100%' }}>
              <QrCode size={16} />
              获取微信二维码
            </Button>
          )}
        </>
      )}
      <div style={{ marginTop: '16px' }}>
        <div className="metric-label" style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
          <Smartphone size={14} />
          安全说明
        </div>
        <p className="page-subtitle" style={{ marginTop: '4px' }}>
          二维码约 4 分钟有效。平台通过官方 iLink 协议扫码获取 bot 凭据，不会读取你的微信其他数据。
        </p>
      </div>
    </Card>
  );

  const renderCredentialCard = (meta: ChannelMeta) => {
    const b = bindingOf(meta.key);
    const bound = !!b && b.state === 'active';
    const f = forms[meta.key] ?? { app_id: '', app_secret: '' };
    return (
      <Card key={meta.key} className="animate-fade-in-up">
        <h3 style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
          <MessageSquare size={18} />
          {meta.label}
          {bound ? <Badge variant="success">已启用</Badge> : <Badge variant="default">未配置</Badge>}
        </h3>
        {bound && b?.meta.app_id && (
          <p className="page-subtitle">
            已配置应用：<code style={{ fontSize: '13px' }}>{b.meta.app_id}</code>
          </p>
        )}
        <div style={{ marginTop: '14px', display: 'grid', gap: '12px' }}>
          <div>
            <label className="metric-label" htmlFor={`${meta.key}-app-id`}>{meta.appIdLabel}</label>
            <input
              id={`${meta.key}-app-id`}
              className="binding-input"
              type="text"
              placeholder={meta.appIdHint}
              value={f.app_id}
              onChange={(e) => setForms((p) => ({ ...p, [meta.key]: { ...f, app_id: e.target.value } }))}
            />
          </div>
          <div>
            <label className="metric-label" htmlFor={`${meta.key}-secret`}>App Secret</label>
            <input
              id={`${meta.key}-secret`}
              className="binding-input"
              type="password"
              placeholder="应用密钥（仅保存，不回显）"
              value={f.app_secret}
              onChange={(e) => setForms((p) => ({ ...p, [meta.key]: { ...f, app_secret: e.target.value } }))}
            />
          </div>
          {formErrors[meta.key] && <span className="input-error">{formErrors[meta.key]}</span>}
          <div style={{ display: 'flex', gap: '10px' }}>
            <Button type="button" onClick={() => handleSave(meta)} isLoading={saving[meta.key]} style={{ flex: 1 }}>
              <Save size={16} />
              保存
            </Button>
            {bound && (
              <Button type="button" variant="danger" onClick={() => handleUnbind(meta)} isLoading={unbinding[meta.key]}>
                <Unlink size={16} />
                解绑
              </Button>
            )}
          </div>
        </div>
        <div className="binding-command" style={{ marginTop: '16px' }}>
          <div className="metric-label" style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
            <Link2 size={14} />
            接入步骤
          </div>
          <ol className="todo-list">
            {meta.help.map((h) => (
              <li key={h}>{h}</li>
            ))}
          </ol>
        </div>
      </Card>
    );
  };

  return (
    <div className="page">
      <header className="page-header animate-fade-in-up">
        <div>
          <h1>渠道绑定</h1>
          <p className="page-subtitle">配置各 IM 渠道连接，保存即生效（无需测试按钮，状态轮询自动更新）</p>
        </div>
        <Badge variant="default">IM</Badge>
      </header>

      {error && <span className="input-error" style={{ marginBottom: '16px', display: 'block' }}>{error}</span>}

      {!loaded ? (
        <p className="page-subtitle">加载中...</p>
      ) : (
        <div className="grid">
          <div className="grid grid-2">
            {renderWechatCard()}
            {renderCredentialCard(CREDENTIAL_CHANNELS[0])}
            {renderCredentialCard(CREDENTIAL_CHANNELS[1])}
            {renderCredentialCard(CREDENTIAL_CHANNELS[2])}
          </div>
          <Card className="animate-fade-in-up animate-delay-2">
            <h3 style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
              <Shield size={18} />
              安全说明
            </h3>
            <ol className="todo-list">
              <li>凭据加密存储（AES + key version），接口只返回脱敏值，永不回显 App Secret</li>
              <li>群消息触发遵循各平台协议规则：钉钉 / QQ 必须 @ 机器人，飞书仅申请 @ 群消息权限</li>
              <li>解绑立即断开连接；重新保存后自动热重载，无需重启平台</li>
            </ol>
          </Card>
        </div>
      )}
    </div>
  );
}
