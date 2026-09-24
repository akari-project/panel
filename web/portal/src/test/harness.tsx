// SPDX-License-Identifier: AGPL-3.0-or-later
// 组件测试的公共部分：按“方法 + 路径”应答的模拟服务端，以及在内存路由中渲染整个用户中心。
import { createMemoryHistory } from '@tanstack/react-router';
import { render } from '@testing-library/react';
import { createClientApi } from '@panel/sdk';
import { readPanelConfig } from '@panel/ui';
import { App } from '../app';
import { createPortalI18n } from '../i18n';

export function json(status: number, body: unknown, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': status >= 400 ? 'application/problem+json' : 'application/json', ...headers },
  });
}

export const noContent = () => new Response(null, { status: 204 });

export function problem(status: number, code: string, extra: Record<string, unknown> = {}, headers: Record<string, string> = {}) {
  return json(status, { type: 'about:blank', title: code, status, code, request_id: `req_${code}`, ...extra }, headers);
}

export const me = (over: Record<string, unknown> = {}) => ({
  id: '0192f0c4-0a00-7000-8000-000000000001',
  email: 'alice@example.com',
  is_email_verified: true,
  status: 'active',
  locale: 'zh-CN',
  timezone: 'Asia/Shanghai',
  is_auto_renew: false,
  is_mfa_enabled: false,
  mfa_methods: [],
  entitlement_status: 'none',
  device_limit: 1,
  created_at: '2026-09-01T09:12:00+08:00',
  ...over,
});

export interface Call {
  method: string;
  path: string;
  body: unknown;
}

type Handler = (body: unknown, req: Request) => Response | Promise<Response>;

/** 未列出的路由：GET /v1/me 返回 401（未登录），其余返回 404。 */
export function fakeServer(routes: Record<string, Handler>) {
  const calls: Call[] = [];
  const fetch = async (input: RequestInfo | URL) => {
    const req = input as Request;
    const path = new URL(req.url).pathname;
    const text = await req.text();
    let body: unknown = text;
    try {
      body = text ? JSON.parse(text) : undefined;
    } catch {
      // 表单编码等非 JSON 请求体保留原文。
    }
    calls.push({ method: req.method, path, body });
    const handler = routes[`${req.method} ${path}`];
    if (handler) return handler(body, req);
    if (req.method === 'GET' && path === '/v1/me') return problem(401, 'unauthenticated');
    if (path === '/v1/oauth/token') return json(400, { error: 'invalid_grant' });
    return problem(404, 'not_found');
  };
  const called = (method: string, path: string) => calls.filter((c) => c.method === method && c.path === path);
  return { fetch, calls, called };
}

export function renderPortal(server: { fetch: typeof globalThis.fetch }, path: string, lng: 'zh-CN' | 'en' = 'zh-CN') {
  const history = createMemoryHistory({ initialEntries: [path] });
  render(
    <App
      api={createClientApi({ baseUrl: 'https://api.example.invalid', fetch: server.fetch })}
      config={readPanelConfig({ site_name: 'Akari' })}
      i18n={createPortalI18n(lng)}
      history={history}
    />,
  );
  return history;
}
