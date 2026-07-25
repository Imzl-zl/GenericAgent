import { useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { Input } from '../../components/ui/Input';
import { Button } from '../../components/ui/Button';
import { register } from '../../api/invite';
import { useAuth } from '../../contexts/AuthContext';
import { ApiClientError } from '../../api/client';
import './AuthPage.css';

export function RegisterPage() {
  const navigate = useNavigate();
  const { login } = useAuth();
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [confirmPassword, setConfirmPassword] = useState('');
  const [inviteCode, setInviteCode] = useState('');
  const [error, setError] = useState('');
  const [isLoading, setIsLoading] = useState(false);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError('');
    setIsLoading(true);

    if (!username.trim() || !password || !inviteCode.trim()) {
      setError('用户名、密码和邀请码必填');
      setIsLoading(false);
      return;
    }
    if (password.length < 6) {
      setError('密码至少 6 位');
      setIsLoading(false);
      return;
    }
    if (password !== confirmPassword) {
      setError('两次输入的密码不一致');
      setIsLoading(false);
      return;
    }

    try {
      const res = await register(username.trim(), password, inviteCode.trim());
      localStorage.setItem('ga_user_id', String(res.user_id));
      login(res.username, res.token, false);
      navigate('/app', { replace: true });
    } catch (err) {
      if (err instanceof ApiClientError) {
        setError(`${err.code}: ${err.message}`);
      } else {
        setError('注册失败，请检查邀请码和后端服务');
      }
    } finally {
      setIsLoading(false);
    }
  };

  return (
    <div className="auth-page">
      <div className="auth-container">
        <div className="auth-brand animate-fade-in-up">
          <span className="auth-brand-mark">GA</span>
          <span className="auth-brand-text">Tenant</span>
        </div>
        <div className="auth-card animate-fade-in-up animate-delay-1">
          <div className="auth-scanline" />
          <h1 className="auth-title">注册</h1>
          <p className="auth-subtitle">需要有效邀请码，注册后等待运营者批准</p>
          <form className="auth-form" onSubmit={handleSubmit}>
            <Input
              label="用户名"
              placeholder="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoComplete="username"
            />
            <Input
              label="密码"
              type="password"
              placeholder="******"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="new-password"
            />
            <Input
              label="确认密码"
              type="password"
              placeholder="******"
              value={confirmPassword}
              onChange={(e) => setConfirmPassword(e.target.value)}
              autoComplete="new-password"
            />
            <Input
              label="邀请码"
              placeholder="INVITE-CODE"
              value={inviteCode}
              onChange={(e) => setInviteCode(e.target.value)}
            />
            {error && <span className="input-error">{error}</span>}
            <Button type="submit" isLoading={isLoading} style={{ marginTop: '8px' }}>
              提交注册
            </Button>
          </form>
          <p className="auth-footer">
            已有账号？ <Link to="/login">登录</Link>
          </p>
        </div>
      </div>
    </div>
  );
}
