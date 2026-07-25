import { forwardRef, type InputHTMLAttributes } from 'react';
import './Input.css';

interface InputProps extends InputHTMLAttributes<HTMLInputElement> {
  label?: string;
  error?: string;
}

export const Input = forwardRef<HTMLInputElement, InputProps>(
  ({ label, error, className = '', ...props }, ref) => {
    return (
      <label className={`input-wrapper ${className}`}>
        {label && <span className="input-label">{label}</span>}
        <input
          ref={ref}
          className={`input-field ${error ? 'input-field-error' : ''}`}
          {...props}
        />
        {error && <span className="input-error">{error}</span>}
      </label>
    );
  }
);

Input.displayName = 'Input';
