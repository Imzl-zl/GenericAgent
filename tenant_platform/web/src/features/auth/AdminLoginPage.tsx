import { useState } from 'react';
import { useNavigate, Link } from 'react-router-dom';
import { Input } from '../../components/ui/Input';
import { Button } from '../../components/ui/Button';
import { useAuth } from '../../contexts/AuthContext';
import './AuthPage.css';

export function AdminLoginPage() {
  const navigate = useNavigate();
  const { login: setAuth } = useAuth();
  const [username, setUsername] = useState('');
  const [token, setToken] = useState('');
  const [error, setError] = useState('');
  const [isLoading, setIsLoading] = useState(false);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError('');

    if (!username.trim()) {
      setError('用户名必填');
      return;
    }

    if (!token.trim()) {
      setError('Admin Token 必填');
      return;
    }

    setIsLoading(true);
    try {
      setAuth(username.trim(), token.trim(), true);
      navigate('/admin', { replace: true });
    } catch {
      setError('登录失败');
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
          <h1 className="auth-title">运营者登录</h1>
          <p className="auth-subtitle">使用平台 Admin Token 进入运营后台</p>
          <form className="auth-form" onSubmit={handleSubmit}>
            <Input
              label="用户名"
              placeholder="username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoComplete="username"
            />
            <Input
              label="Admin Token"
              placeholder="PLATFORM_ADMIN_TOKEN"
              value={token}
              onChange={(e) => setToken(e.target.value)}
            />
            {error && <span className="input-error">{error}</span>}
            <Button type="submit" isLoading={isLoading} style={{ marginTop: '8px' }}>
              登录
            </Button>
          </form>
          <p className="auth-footer auth-footer-link">
            <Link to="/login">返回普通用户登录</Link>
          </p>
        </div>
      </div>
    </div>
  );
}
