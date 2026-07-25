import type { CSSProperties, ReactNode } from 'react';
import './Card.css';

interface CardProps {
  children: ReactNode;
  className?: string;
  variant?: 'default' | 'interactive' | 'glow';
  style?: CSSProperties;
}

const variantClass: Record<NonNullable<CardProps['variant']>, string> = {
  default: 'card-default',
  interactive: 'card-interactive',
  glow: 'card-glow',
};

export function Card({ children, className = '', variant = 'default', style }: CardProps) {
  return (
    <div className={`card ${variantClass[variant]} ${className}`} style={style}>
      {children}
    </div>
  );
}
