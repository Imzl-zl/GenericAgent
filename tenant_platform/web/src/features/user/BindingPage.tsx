import { useEffect, useState, useRef } from 'react';
import { Card } from '../../components/ui/Card';
import { Button } from '../../components/ui/Button';
import { Badge } from '../../components/ui/Badge';
import { QrCode, RefreshCw, Shield, Smartphone, Clock } from 'lucide-react';
import { QRCodeSVG } from 'qrcode.react';
import {
  createWechatQRCode,
  getWechatQRCodeStatus,
  createAdminWechatQRCode,
  getAdminWechatQRCodeStatus,
  getOwnBot
} from '../../api/bots';
import { ApiClientError } from '../../api/client';
import { useAuth } from '../../contexts/AuthContext';
import type { Bot, WechatQRCode, WechatQRCodeStatus } from '../../api/types';
import './UserPages.css';

const POLL_INTERVAL_MS = 3000;

export function BindingPage() {
  const { state } = useAuth();
  const isAdmin = state?.isAdmin ?? false;

  const [boundBot, setBoundBot] = useState<Bot | null>(null);
  const [qr, setQr] = useState<WechatQRCode | null>(null);
  const [status, setStatus] = useState<WechatQRCodeStatus | null>(null);
  const [error, setError] = useState('');
  const [isLoadingBot, setIsLoadingBot] = useState(true);
  const [isGenerating, setIsGenerating] = useState(false);
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);


  useEffect(() => {
    let active = true;
    void getOwnBot()
      .then((bot) => {
        if (active) setBoundBot(bot);
      })
      .catch(() => {
        if (active) setBoundBot(null);
      })
      .finally(() => {
        if (active) setIsLoadingBot(false);
      });
    return () => {
      active = false;
      clearTimeout(pollRef.current ?? undefined);
    };
  }, []);

  const startPolling = (qrcodeToken: string) => {
    stopPolling();
    const tick = async () => {
      try {
        // 根据用户类型调用不同的 API
        const s = isAdmin
          ? await getAdminWechatQRCodeStatus(qrcodeToken)
          : await getWechatQRCodeStatus(qrcodeToken);
        setStatus(s);
        if (s.status === 'confirmed' || s.status === 'expired') {
          return; // 结束轮询（不排下一轮）
        }
        if (s.bound && s.bot) {
          setBoundBot(s.bot);
        }
      } catch (err) {
        // 单次轮询失败不能停止：confirmed 只在这一刻可捕捉，错过即永久
        // 丢失（平台无后端兜底）。继续轮询直到 expired/confirmed。
        setError(bindingErrorText(err, '轮询状态失败'));
      }
      // 串行轮询：等上一轮完成后再隔 3s 排下一轮（避免并发堆积）
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
    setStatus(null);
    setBoundBot(null);
    stopPolling();
    setIsGenerating(true);
    try {
      // 根据用户类型调用不同的 API
      const code = isAdmin
        ? await createAdminWechatQRCode()
        : await createWechatQRCode();
      setQr(code);
      startPolling(code.qrcode_token);
    } catch (err) {
      setError(bindingErrorText(err, '获取微信二维码失败'));
    } finally {
      setIsGenerating(false);
    }
  };

  const bindingErrorText = (err: unknown, fallback: string) => {
    if (err instanceof ApiClientError) {
      if (err.code === 'FEATURE_DISABLED') {
        return '微信绑定功能未启用（未配置 iLink，请联系管理员）';
      }
      return `${err.code}: ${err.message}`;
    }
    return fallback;
  };

  const formatExpiry = (s: string) => {
    const d = new Date(s);
    return d.toLocaleString('zh-CN', { hour12: false });
  };

  const statusLabel = (s?: string) => {
    switch (s) {
      case 'wait':
        return '等待扫码';
      case 'scaned':
        return '已扫码，等待确认';
      case 'scaned_but_redirect':
        return '扫码成功，正在连接';
      case 'expired':
        return '二维码已过期';
      case 'confirmed':
        return '绑定成功';
      default:
        return '未知';
    }
  };

  if (!isLoadingBot && boundBot && !qr) {
    return (
      <div className="page">
        <header className="page-header animate-fade-in-up">
          <div>
            <h1>微信绑定</h1>
            <p className="page-subtitle">你的微信 bot 已绑定</p>
          </div>
          <Badge variant="success">已绑定</Badge>
        </header>
        <Card className="animate-fade-in-up animate-delay-1">
          <div style={{ display: 'grid', gap: '16px' }}>
            <div>
              <div className="metric-label">iLink Bot ID</div>
              <code style={{ fontSize: '16px' }}>{boundBot.ilink_bot_id}</code>
            </div>
            <div>
              <div className="metric-label">iLink User ID</div>
              <code style={{ fontSize: '14px', color: 'var(--text-muted)' }}>{boundBot.ilink_user_id}</code>
            </div>
            <div>
              <div className="metric-label">状态</div>
              <Badge variant="success">{boundBot.state}</Badge>
            </div>
          </div>
        </Card>
      </div>
    );
  }

  return (
    <div className="page">
      <header className="page-header animate-fade-in-up">
        <div>
          <h1>微信绑定</h1>
          <p className="page-subtitle">使用微信官方 iLink 扫码登录完成绑定</p>
        </div>
        <Badge variant="default">P0</Badge>
      </header>

      <div className="grid grid-2">
        <Card className="animate-fade-in-up animate-delay-1">
          <h3 style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
            <QrCode size={18} />
            微信二维码
          </h3>
          <p className="page-subtitle">点击获取二维码，用微信扫描授权</p>

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
              {status && (
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
                  <Badge variant={status.status === 'confirmed' ? 'success' : status.status === 'expired' ? 'danger' : 'default'}>
                    {statusLabel(status.status)}
                  </Badge>
                </div>
              )}
              {(status?.status === 'scaned' || status?.status === 'scaned_but_redirect') && (
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
          {error && <span className="input-error" style={{ marginTop: '16px', display: 'block' }}>{error}</span>}
        </Card>

        <Card className="animate-fade-in-up animate-delay-2">
          <h3 style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
            <Shield size={18} />
            绑定步骤
          </h3>
          <ol className="todo-list">
            <li>点击左侧按钮，获取微信官方二维码</li>
            <li>打开微信，使用“扫一扫”扫描页面二维码</li>
            <li>在微信中点击确认授权</li>
            <li>保持本页面打开，自动检测绑定结果，成功后即可开始对话</li>
          </ol>
          <div className="binding-command" style={{ marginTop: '20px' }}>
            <div className="metric-label" style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
              <Smartphone size={14} />
              安全说明
            </div>
            <p className="page-subtitle" style={{ marginTop: '4px' }}>
              二维码约 4 分钟有效，过期后需重新获取。平台通过官方 iLink 协议扫码获取 bot 凭据，不会读取你的微信其他数据。
            </p>
          </div>
        </Card>
      </div>
    </div>
  );
}
