import { useCallback, useState, type ReactNode } from 'react';
import { AuthContext, type AuthState } from './AuthContext';

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
    };
  });

  const login = useCallback((username: string, token: string, isAdmin = false) => {
    localStorage.removeItem('ga_username');
    localStorage.removeItem('ga_admin_token');
    localStorage.removeItem('ga_user_token');
    localStorage.removeItem('ga_is_admin');
    localStorage.removeItem('ga_user_id');

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
