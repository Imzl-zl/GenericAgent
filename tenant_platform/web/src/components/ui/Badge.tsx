import './Badge.css';

type BadgeVariant = 'default' | 'success' | 'warning' | 'danger' | 'info';

interface BadgeProps {
  children: string;
  variant?: BadgeVariant;
}

const variantClass: Record<BadgeVariant, string> = {
  default: 'badge-default',
  success: 'badge-success',
  warning: 'badge-warning',
  danger: 'badge-danger',
  info: 'badge-info',
};

export function Badge({ children, variant = 'default' }: BadgeProps) {
  return <span className={`badge ${variantClass[variant]}`}>{children}</span>;
}
