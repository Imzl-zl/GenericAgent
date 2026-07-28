import type { ApiError } from './types';

const API_BASE = import.meta.env.VITE_API_BASE_URL || '';

class ApiClientError extends Error {
  status: number;
  code: string;
  traceId: string;

  constructor(message: string, status: number, code: string, traceId: string) {
    super(message);
    this.status = status;
    this.code = code;
    this.traceId = traceId;
  }
}

export function getAdminToken(): string | null {
  return localStorage.getItem('ga_admin_token');
}

export function getUserToken(): string | null {
  return localStorage.getItem('ga_user_token');
}

function requestOptions(tokenType: 'admin' | 'user' | null): Record<string, string> {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
  };
  if (tokenType === 'admin') {
    const token = getAdminToken();
    if (token) {
      headers['X-Platform-Dev-Token'] = token;
    }
  } else if (tokenType === 'user') {
    const token = getUserToken();
    if (token) {
      headers['Authorization'] = `Bearer ${token}`;
    }
  }
  return headers;
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  tokenType: 'admin' | 'user' | null = 'admin'
): Promise<T> {
  const res = await fetch(`${API_BASE}${path}`, {
    method,
    headers: requestOptions(tokenType),
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });

  if (!res.ok) {
    const data = (await res.json().catch(() => ({}))) as Partial<ApiError>;
    throw new ApiClientError(
      data.message || res.statusText,
      res.status,
      data.code || 'UNKNOWN',
      data.trace_id || ''
    );
  }

  if (res.status === 204) {
    return undefined as T;
  }
  return (await res.json()) as T;
}

export const api = {
  get: <T>(path: string) => request<T>('GET', path),
  post: <T>(path: string, body?: unknown) => request<T>('POST', path, body),
  put: <T>(path: string, body?: unknown) => request<T>('PUT', path, body),
  delete: <T>(path: string, body?: unknown) => request<T>('DELETE', path, body),
};

export const userApi = {
  get: <T>(path: string) => request<T>('GET', path, undefined, 'user'),
  post: <T>(path: string, body?: unknown) => request<T>('POST', path, body, 'user'),
  put: <T>(path: string, body?: unknown) => request<T>('PUT', path, body, 'user'),
  delete: <T>(path: string) => request<T>('DELETE', path, undefined, 'user'),
};

export { ApiClientError };
