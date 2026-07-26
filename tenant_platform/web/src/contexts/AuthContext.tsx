import { createContext, useContext, useState, useCallback, type ReactNode } from 'react';

interface AuthState {
  username: string;
  isAdmin: boolean;
  adminToken?: string;
  userToken?: string;
}

interface AuthContextValue {
  state: AuthState | null;
  login: (username: string, token: string, isAdmin?: boolean) => void;
  logout: () => void;
}

const AuthContext = createContext<AuthContextValue | undefined>(undefined);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState | null>(() => {
    const username = localStorage.getItem('ga_username');
    const adminToken = localStorage.getItem('ga_admin_token');
    const userToken = localStorage.getItem('ga_user_token');
    const isAdmin = localStorage.getItem('ga_is_admin') === 'true';
    if (!username) {
      return null;
    }
    return { username, isAdmin, adminToken: adminToken || undefined, userToken: userToken || undefined };
  });

  const login = useCallback((username: string, token: string, isAdmin = false) => {
    // 完全清空之前的登录状态，防止串用
    localStorage.removeItem('ga_username');
    localStorage.removeItem('ga_admin_token');
    localStorage.removeItem('ga_user_token');
    localStorage.removeItem('ga_is_admin');
    localStorage.removeItem('ga_user_id');

    // 设置新的登录状态
    localStorage.setItem('ga_username', username);
    localStorage.setItem('ga_is_admin', String(isAdmin));
    if (isAdmin) {
      localStorage.setItem('ga_admin_token', token);
      setState({ username, isAdmin, adminToken: token });
    } else {
      localStorage.setItem('ga_user_token', token);
      setState({ username, isAdmin, userToken: token });
    }
  }, []);

  const logout = useCallback(() => {
    localStorage.removeItem('ga_username');
    localStorage.removeItem('ga_admin_token');
    localStorage.removeItem('ga_user_token');
    localStorage.removeItem('ga_is_admin');
    localStorage.removeItem('ga_user_id');
    setState(null);
  }, []);

  return (
    <AuthContext.Provider value={{ state, login, logout }}>
      {children}
    </AuthContext.Provider>
  );
}

export function useAuth() {
  const ctx = useContext(AuthContext);
  if (!ctx) {
    throw new Error('useAuth must be used within AuthProvider');
  }
  return ctx;
}
