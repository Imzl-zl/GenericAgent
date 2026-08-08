import { useCallback, useState, type ReactNode } from 'react';
import { AuthContext, type AuthState } from './AuthContext';
import { getMe } from '../api/users';

const USER_STATUS_KEY = 'ga_user_status';

function readStoredStatus(): 'pending' | 'approved' | 'blocked' | undefined {
  const v = localStorage.getItem(USER_STATUS_KEY);
  return v === 'pending' || v === 'approved' || v === 'blocked' ? v : undefined;
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState | null>(() => {
    const username = localStorage.getItem('ga_username');
    const adminToken = localStorage.getItem('ga_admin_token');
    const userToken = localStorage.getItem('ga_user_token');
    const isAdmin = localStorage.getItem('ga_is_admin') === 'true';
    if (!username) {
      return null;
    }
    return {
      username,
      isAdmin,
      adminToken: adminToken || undefined,
      userToken: userToken || undefined,
      userId: undefined,
      status: readStoredStatus(),
    };
  });

  const login = useCallback((username: string, token: string, isAdmin = false, extra?: { userId?: number; status?: 'pending' | 'approved' | 'blocked' }) => {
    localStorage.removeItem('ga_username');
    localStorage.removeItem('ga_admin_token');
    localStorage.removeItem('ga_user_token');
    localStorage.removeItem('ga_is_admin');
    localStorage.removeItem(USER_STATUS_KEY);

    localStorage.setItem('ga_username', username);
    localStorage.setItem('ga_is_admin', String(isAdmin));
    if (extra?.status) {
      localStorage.setItem(USER_STATUS_KEY, extra.status);
    }
    if (isAdmin) {
      localStorage.setItem('ga_admin_token', token);
      setState({ username, isAdmin, adminToken: token, userId: extra?.userId, status: extra?.status });
    } else {
      localStorage.setItem('ga_user_token', token);
      setState({ username, isAdmin, userToken: token, userId: extra?.userId, status: extra?.status });
    }
  }, []);

  const logout = useCallback(() => {
    localStorage.removeItem('ga_username');
    localStorage.removeItem('ga_admin_token');
    localStorage.removeItem('ga_user_token');
    localStorage.removeItem('ga_is_admin');
    localStorage.removeItem(USER_STATUS_KEY);
    setState(null);
  }, []);

  // 只对普通用户生效（getMe 是 userAuth 保护的用户端点）；管理员无状态字段，
  // 调用失败静默忽略（如未登录/网络错误），保持现有展示。
  const refreshStatus = useCallback(async () => {
    const userToken = localStorage.getItem('ga_user_token');
    const username = localStorage.getItem('ga_username');
    if (!userToken || !username) {
      return;
    }
    try {
      const me = await getMe();
      localStorage.setItem(USER_STATUS_KEY, me.status);
      setState((prev) => (prev ? { ...prev, userId: me.user_id, status: me.status } : prev));
    } catch {
      // 忽略：状态刷新失败不打断页面，仍展示上次已知状态
    }
  }, []);

  return (
    <AuthContext.Provider value={{ state, login, logout, refreshStatus }}>
      {children}
    </AuthContext.Provider>
  );
}
