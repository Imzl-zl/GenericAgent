import { createContext, useContext } from 'react';

export interface AuthState {
  username: string;
  isAdmin: boolean;
  adminToken?: string;
  userToken?: string;
  userId?: number;
  status?: 'pending' | 'approved' | 'blocked';
}

export interface AuthContextValue {
  state: AuthState | null;
  login: (username: string, token: string, isAdmin?: boolean, extra?: { userId?: number; status?: 'pending' | 'approved' | 'blocked' }) => void;
  logout: () => void;
  /** 从后端重新拉取当前用户状态（管理员审批后前端状态是登录时快照，需刷新） */
  refreshStatus: () => Promise<void>;
}

export const AuthContext = createContext<AuthContextValue | undefined>(undefined);


export function useAuth() {
  const ctx = useContext(AuthContext);
  if (!ctx) {
    throw new Error('useAuth must be used within AuthProvider');
  }
  return ctx;
}
