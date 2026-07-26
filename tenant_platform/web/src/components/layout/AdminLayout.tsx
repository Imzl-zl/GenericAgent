import { NavLink, Outlet, useNavigate } from 'react-router-dom';
import { Shield, Users, Ticket, UserCog, Cpu, Settings, LogOut, MessageCircle } from 'lucide-react';
import { useAuth } from '../../contexts/AuthContext';
import './Layout.css';

const navItems = [
  { to: '/admin', label: '控制台', icon: Shield },
  { to: '/admin/users', label: '用户审批', icon: Users },
  { to: '/admin/invite-codes', label: '邀请码', icon: Ticket },
  { to: '/admin/personas', label: '人设审核', icon: UserCog },
  { to: '/admin/providers', label: 'LLM 供应', icon: Cpu },
  { to: '/admin/binding', label: '微信绑定', icon: MessageCircle },
  { to: '/admin/settings', label: '策略设置', icon: Settings },
];

export function AdminLayout() {
  const { logout } = useAuth();
  const navigate = useNavigate();

  const handleLogout = () => {
    logout();
    navigate('/login', { replace: true });
  };

  return (
    <div className="layout">
      <aside className="layout-sidebar layout-sidebar-admin">
        <div className="layout-brand">
          <span className="layout-brand-mark layout-brand-mark-admin">ADM</span>
          <span className="layout-brand-text">Control</span>
        </div>
        <nav className="layout-nav">
          {navItems.map(({ to, label, icon: Icon }) => (
            <NavLink key={to} to={to} end={to === '/admin'} className="layout-nav-item">
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
