import { API_TOKEN_STORAGE_KEY } from '../services/api.service.js';

export class WorkspaceApiError extends Error {
  constructor(message, status, code, details) {
    super(message);
    this.name = 'WorkspaceApiError';
    this.status = status;
    this.code = code || null;
    this.details = details || null;
  }
}

function tokenFromStorage() {
  try {
    return localStorage.getItem(API_TOKEN_STORAGE_KEY) || '';
  } catch {
    return '';
  }
}

export async function request(path, options = {}) {
  const url = new URL(path, window.location.origin);
  if (url.origin !== window.location.origin ||
      !(/^\/api\/|^\/health\/|^\/oauth\//.test(url.pathname))) {
    throw new TypeError('Workspace API requests must use a same-origin API route.');
  }

  const headers = new Headers(options.headers || {});
  headers.set('Accept', 'application/json');
  const token = tokenFromStorage();
  if (token) headers.set('Authorization', 'Bearer ' + token);

  const init = { method: options.method || 'GET', headers, signal: options.signal };
  if (options.body !== undefined) {
    headers.set('Content-Type', 'application/json');
    init.body = JSON.stringify(options.body);
  }

  let response;
  try {
    response = await fetch(url, init);
  } catch {
    throw new WorkspaceApiError('Cannot reach the Wave Ray server. Check that it is running and try again.', 0, 'network_unavailable');
  }

  const contentType = response.headers.get('content-type') || '';
  let payload = null;
  if (contentType.includes('application/json')) {
    payload = await response.json().catch(() => null);
  } else {
    payload = await response.text().catch(() => '');
  }

  if (!response.ok) {
    const message = payload && typeof payload === 'object'
      ? (payload.error || payload.message || payload.detail || response.statusText)
      : (typeof payload === 'string' && payload ? payload : response.statusText);
    throw new WorkspaceApiError(
      String(message || 'Request failed'),
      response.status,
      payload && typeof payload === 'object' ? payload.error_code || payload.code : null,
      payload
    );
  }
  return payload;
}

export const api = {
  get(path, options) {
    return request(path, options);
  },
  post(path, body, options) {
    return request(path, { ...options, method: 'POST', body });
  },
  delete(path, options) {
    return request(path, { ...options, method: 'DELETE' });
  }
};
