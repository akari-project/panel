// SPDX-License-Identifier: AGPL-3.0-or-later
import { createMemoryHistory } from '@tanstack/react-router';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { createClientApi } from '@panel/sdk';
import { readPanelConfig } from '@panel/ui';
import { App } from './app';
import { createPortalI18n } from './i18n';
import { safeRedirect } from './router';

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });
}

// 模拟服务端：登录后 /v1/me 返回账号，否则 401。
function fakeServer() {
  let signedIn = false;
  const calls: string[] = [];
  const fetch = async (input: RequestInfo | URL) => {
    const req = input as Request;
    const path = new URL(req.url).pathname;
    calls.push(`${req.method} ${path}`);
    if (req.method === 'GET' && path === '/v1/me') {
      return signedIn ? json(200, { email: 'alice@example.com' }) : json(401, { code: 'unauthenticated', request_id: 'r1' });
    }
    if (req.method === 'POST' && path === '/v1/sessions') {
      const body = (await req.json()) as { device: { platform: string } };
      expect(body.device.platform).toBe('web');
      signedIn = true;
      return json(201, { device_id: 'd', token_type: 'Bearer', expires_in: 900, credential_status: 'web_device' });
    }
    return json(404, { code: 'not_found' });
  };
  return { fetch, calls };
}

describe('portal', () => {
  it('未登录跳转到登录页，登录后进入首页', async () => {
    const server = fakeServer();
    render(
      <App
        api={createClientApi({ baseUrl: 'https://api.example.invalid', fetch: server.fetch })}
        config={readPanelConfig({ site_name: 'Akari' })}
        i18n={createPortalI18n('zh-CN')}
        history={createMemoryHistory({ initialEntries: ['/'] })}
      />,
    );
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('邮箱'), 'alice@example.com');
    await user.type(screen.getByLabelText('密码'), 'secret');
    await user.click(screen.getByRole('button', { name: '登录' }));
    expect(await screen.findByRole('heading', { name: '概览' })).toBeInTheDocument();
    expect(screen.getByText('欢迎，alice@example.com')).toBeInTheDocument();
    expect(server.calls).toContain('POST /v1/sessions');
  });
});

describe('safeRedirect', () => {
  it('只接受站内路径', () => {
    expect(safeRedirect('/orders?x=1')).toBe('/orders?x=1');
    expect(safeRedirect('//evil.example')).toBeUndefined();
    expect(safeRedirect('https://evil.example')).toBeUndefined();
    expect(safeRedirect(1)).toBeUndefined();
  });
});
