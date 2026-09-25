// SPDX-License-Identifier: AGPL-3.0-or-later
// 组件测试的公共部分：按“方法 + 路径”应答的模拟管理接口，以及在内存路由中渲染整个管理后台。
import { createMemoryHistory } from '@tanstack/react-router';
import { render } from '@testing-library/react';
import { createConsoleApi } from '@panel/sdk';
import { readPanelConfig } from '@panel/ui';
import { App } from '../app';
import { createAdminI18n } from '../i18n';

export function json(status: number, body: unknown, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': status >= 400 ? 'application/problem+json' : 'application/json', ...headers },
  });
}

export const noContent = () => new Response(null, { status: 204 });

export function problem(status: number, code: string, extra: Record<string, unknown> = {}) {
  return json(status, { type: 'about:blank', title: code, status, code, request_id: `req_${code}`, ...extra });
}

export const superadmin = {
  account_id: '01927c3e-8a41-7003-9d3e-5f6a7b8c0003',
  email: 'root@example.com',
  roles: ['superadmin'],
  permissions: ['*'],
  is_superadmin: true,
  has_totp: true,
  has_passkey: false,
};

export const support = {
  ...superadmin,
  email: 'support@example.com',
  roles: ['support'],
  permissions: ['accounts.read', 'orders.read', 'tickets.*'],
  is_superadmin: false,
};

export interface Call {
  method: string;
  path: string;
  search: string;
  body: unknown;
  headers: Headers;
}

type Handler = (body: unknown, req: Request) => Response | Promise<Response>;

/** 未列出的路由返回 404；GET /v1/staff/me 默认返回 me。 */
export function fakeServer(me: object | null, routes: Record<string, Handler> = {}) {
  const calls: Call[] = [];
  const fetch = async (input: RequestInfo | URL) => {
    const req = input as Request;
    const url = new URL(req.url);
    const text = await req.text();
    let body: unknown = text;
    try {
      body = text ? JSON.parse(text) : undefined;
    } catch {
      // 表单编码等非 JSON 请求体保留原文。
    }
    calls.push({ method: req.method, path: url.pathname, search: url.search, body, headers: req.headers });
    const handler = routes[`${req.method} ${url.pathname}`];
    if (handler) return handler(body, req);
    if (req.method === 'GET' && url.pathname === '/v1/staff/me') return me ? json(200, me) : problem(401, 'unauthenticated');
    if (url.pathname === '/v1/oauth/token') return json(400, { error: 'invalid_grant' });
    return problem(404, 'not_found');
  };
  const called = (method: string, path: string) => calls.filter((c) => c.method === method && c.path === path);
  return { fetch, calls, called };
}

export function renderAdmin(server: { fetch: typeof globalThis.fetch }, path: string, lng: 'zh-CN' | 'en' = 'zh-CN') {
  const history = createMemoryHistory({ initialEntries: [`/admin${path}`] });
  render(
    <App
      api={createConsoleApi({ baseUrl: 'https://console.example.invalid', fetch: server.fetch })}
      config={readPanelConfig({ site_name: 'Akari Console' }, '/admin/')}
      i18n={createAdminI18n(lng)}
      history={history}
    />,
  );
  return history;
}
