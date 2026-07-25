import { useState } from 'react';
import { useNavigate, Link } from 'react-router-dom';
import { Input } from '../../components/ui/Input';
import { Button } from '../../components/ui/Button';
import { useAuth } from '../../contexts/AuthContext';
import { login } from '../../api/invite';
import { ApiClientError } from '../../api/client';
import './AuthPage.css';

export function LoginPage() {
  const navigate = useNavigate();
  const { login: setAuth } = useAuth();
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [token, setToken] = useState('');
  const [isAdmin, setIsAdmin] = useState(false);
  const [error, setError] = useState('');
  const [isLoading, setIsLoading] = useState(false);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError('');
    setIsLoading(true);

    if (!username.trim()) {
      setError('用户名必填');
      setIsLoading(false);
      return;
    }

    try {
      if (isAdmin) {
        if (!token.trim()) {
          setError('Dev Token 必填');
          setIsLoading(false);
          return;
        }
        setAuth(username.trim(), token.trim(), true);
        navigate('/admin', { replace: true });
      } else {
        if (!password) {
          setError('密码必填');
          setIsLoading(false);
          return;
        }
        const res = await login(username.trim(), password);
        localStorage.setItem('ga_user_id', String(res.user_id));
        setAuth(res.username, res.token, false);
        navigate('/app', { replace: true });
      }
    } catch (err) {
      if (err instanceof ApiClientError) {
        setError(`${err.code}: ${err.message}`);
      } else {
        setError(isAdmin ? '登录失败' : '用户名或密码错误');
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
          <h1 className="auth-title">登录</h1>
          <p className="auth-subtitle">{isAdmin ? '使用平台 Dev Token 进入运营后台' : '使用用户名和密码登录'}</p>
          <form className="auth-form" onSubmit={handleSubmit}>
            <Input
              label="用户名"
              placeholder="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoComplete="username"
            />
            {isAdmin ? (
              <Input
                label="Dev Token"
                placeholder="PLATFORM_DEV_TOKEN"
                value={token}
                onChange={(e) => setToken(e.target.value)}
              />
            ) : (
              <Input
                label="密码"
                type="password"
                placeholder="******"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                autoComplete="current-password"
              />
            )}
            <label className="admin-switch">
              <input
                type="checkbox"
                checked={isAdmin}
                onChange={(e) => setIsAdmin(e.target.checked)}
              />
              <span>以运营者身份登录</span>
            </label>
            {error && <span className="input-error">{error}</span>}
            <Button type="submit" isLoading={isLoading} style={{ marginTop: '8px' }}>
              登录
            </Button>
          </form>
          <p className="auth-footer">
            还没有账号？ <Link to="/register">注册</Link>
          </p>
        </div>
      </div>
    </div>
  );
}
