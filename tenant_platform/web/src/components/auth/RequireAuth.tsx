import { Navigate } from 'react-router-dom';
import { useAuth } from '../../contexts/AuthContext';
import type { ReactNode } from 'react';

interface RequireAuthProps {
  children: ReactNode;
  adminOnly?: boolean;
}

export function RequireAuth({ children, adminOnly }: RequireAuthProps) {
  const { state } = useAuth();

  // 未登录，跳转到对应的登录页
  if (!state) {
    return <Navigate to={adminOnly ? '/admin/login' : '/login'} replace />;
  }

  // 要求管理员权限，但当前不是管理员登录
  if (adminOnly && !state.isAdmin) {
    return <Navigate to="/login" replace />;
  }

  // 要求管理员权限，但没有管理员 token
  if (adminOnly && !state.adminToken) {
    return <Navigate to="/admin/login" replace />;
  }

  // 要求普通用户权限，但当前是管理员登录
  if (!adminOnly && state.isAdmin) {
    return <Navigate to="/admin" replace />;
  }

  // 要求普通用户权限，但没有用户 token
  if (!adminOnly && !state.userToken) {
    return <Navigate to="/login" replace />;
  }

  return <>{children}</>;
}
