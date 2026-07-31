import { createBrowserRouter, Navigate } from 'react-router-dom';
import { AppLayout } from '../components/layout/AppLayout';
import { AdminLayout } from '../components/layout/AdminLayout';
import { RequireAuth } from '../components/auth/RequireAuth';
import { LoginPage } from '../features/auth/LoginPage';
import { AdminLoginPage } from '../features/auth/AdminLoginPage';
import { RegisterPage } from '../features/auth/RegisterPage';
import { DashboardPage } from '../features/user/DashboardPage';
import { BindingPage } from '../features/user/BindingPage';
import { PersonaPage } from '../features/user/PersonaPage';
import { StatusPage } from '../features/user/StatusPage';
import { SOPLibraryPage } from '../features/user/SOPLibraryPage';
import { DocsPage } from '../features/docs/DocsPage';
import { AdminDashboardPage } from '../features/admin/AdminDashboardPage';
import { UsersPage } from '../features/admin/UsersPage';
import { InviteCodesPage } from '../features/admin/InviteCodesPage';
import { PersonaReviewPage } from '../features/admin/PersonaReviewPage';
import { LLMProvidersPage } from '../features/admin/LLMProvidersPage';
import { MCPServersPage } from '../features/admin/MCPServersPage';
import { SOPAdminPage } from '../features/admin/SOPAdminPage';
import { SettingsPage } from '../features/admin/SettingsPage';


export const router = createBrowserRouter([
  {
    path: '/',
    element: <Navigate to="/app" replace />,
  },
  {
    path: '/login',
    element: <LoginPage />,
  },
  {
    path: '/admin/login',
    element: <AdminLoginPage />,
  },
  {
    path: '/register',
    element: <RegisterPage />,
  },
  {
    path: '/app',
    element: <RequireAuth><AppLayout /></RequireAuth>,
    children: [
      { index: true, element: <DashboardPage /> },
      { path: 'binding', element: <BindingPage /> },
      { path: 'persona', element: <PersonaPage /> },
      { path: 'status', element: <StatusPage /> },
      { path: 'sops', element: <SOPLibraryPage /> },
      { path: 'docs', element: <DocsPage /> },
    ],
  },
  {
    path: '/admin',
    element: <RequireAuth adminOnly><AdminLayout /></RequireAuth>,
    children: [
      { index: true, element: <AdminDashboardPage /> },
      { path: 'users', element: <UsersPage /> },
      { path: 'invite-codes', element: <InviteCodesPage /> },
      { path: 'personas', element: <PersonaReviewPage /> },
      { path: 'providers', element: <LLMProvidersPage /> },
      { path: 'mcp-servers', element: <MCPServersPage /> },
      { path: 'sops', element: <SOPAdminPage /> },
      { path: 'binding', element: <BindingPage /> },
      { path: 'settings', element: <SettingsPage /> },
    ],
  },
]);
