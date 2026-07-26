import { Navigate } from 'react-router-dom';
import { useAuth } from '../../contexts/AuthContext';
import type { ReactNode } from 'react';

interface RequireAuthProps {
  children: ReactNode;
  adminOnly?: boolean;
}

export function RequireAuth({ children, adminOnly }: RequireAuthProps) {
  const { state } = useAuth();

  if (!state) {
    return <Navigate to={adminOnly ? '/admin/login' : '/login'} replace />;
  }

  if (adminOnly && (!state.isAdmin || !state.adminToken)) {
    return <Navigate to="/app" replace />;
  }

  if (!adminOnly && !state.userToken && !state.isAdmin) {
    return <Navigate to="/login" replace />;
  }

  return <>{children}</>;
}
