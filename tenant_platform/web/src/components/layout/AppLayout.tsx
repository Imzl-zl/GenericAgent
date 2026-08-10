import { NavLink, Outlet, useNavigate } from 'react-router-dom';
import { LayoutDashboard, Link2, UserCog, Activity, BookOpen, LogOut } from 'lucide-react';
import { useAuth } from '../../contexts/AuthContext';
import './Layout.css';

const navItems = [
  { to: '/app', label: '仪表盘', icon: LayoutDashboard },
  { to: '/app/binding', label: '渠道绑定', icon: Link2 },
  { to: '/app/persona', label: '人设', icon: UserCog },
  { to: '/app/status', label: '运行状态', icon: Activity },
  { to: '/app/docs', label: '使用文档', icon: BookOpen },
];

export function AppLayout() {
  const { logout } = useAuth();
  const navigate = useNavigate();

  const handleLogout = () => {
    logout();
    navigate('/login', { replace: true });
  };

  return (
    <div className="layout">
      <aside className="layout-sidebar">
        <div className="layout-brand">
          <span className="layout-brand-mark">GA</span>
          <span className="layout-brand-text">Tenant</span>
        </div>
        <nav className="layout-nav">
          {navItems.map(({ to, label, icon: Icon }) => (
            <NavLink key={to} to={to} end={to === '/app'} className="layout-nav-item">
              <Icon size={18} strokeWidth={1.5} />
              <span>{label}</span>
            </NavLink>
          ))}
        </nav>
        <div className="layout-footer">
          <button className="layout-nav-item layout-logout" type="button" onClick={handleLogout}>
            <LogOut size={18} strokeWidth={1.5} />
            <span>退出</span>
          </button>
        </div>
      </aside>
      <main className="layout-main">
        <Outlet />
      </main>
    </div>
  );
}
