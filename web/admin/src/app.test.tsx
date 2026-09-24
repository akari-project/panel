// SPDX-License-Identifier: AGPL-3.0-or-later
import { createMemoryHistory } from '@tanstack/react-router';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { createConsoleApi } from '@panel/sdk';
import { readPanelConfig } from '@panel/ui';
import { App } from './app';
import { createAdminI18n } from './i18n';

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });
}

// 模拟服务端：提交密码返回 mfa_required，提交验证码后登录成功（AUTH-21）。
function fakeServer() {
  let signedIn = false;
  const fetch = async (input: RequestInfo | URL) => {
    const req = input as Request;
    const path = new URL(req.url).pathname;
    if (req.method === 'GET' && path === '/v1/staff/me') {
      return signedIn ? json(200, { email: 'ops@example.com' }) : json(401, { code: 'unauthenticated' });
    }
    if (req.method === 'POST' && path === '/v1/sessions') {
      const body = (await req.json()) as Record<string, unknown>;
      if ('password' in body) {
        return json(401, { code: 'mfa_required', request_id: 'r1', challenge_id: 'c1', methods: ['totp', 'recovery_code'] });
      }
      if (body.challenge_id === 'c1' && body.totp_code === '492871') {
        signedIn = true;
        return json(201, { session_id: 's' });
      }
      return json(401, { code: 'unauthenticated', request_id: 'r2' });
    }
    return json(404, { code: 'not_found' });
  };
  return { fetch };
}

describe('admin', () => {
  it('两步登录后进入首页；验证码错误时显示错误与请求编号', async () => {
    const server = fakeServer();
    render(
      <App
        api={createConsoleApi({ baseUrl: 'https://console.example.invalid', fetch: server.fetch })}
        config={readPanelConfig({ site_name: 'Akari Console' }, '/admin/')}
        i18n={createAdminI18n('zh-CN')}
        history={createMemoryHistory({ initialEntries: ['/admin/'] })}
      />,
    );
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('邮箱'), 'ops@example.com');
    await user.type(screen.getByLabelText('密码'), 'correct-horse-battery');
    await user.click(screen.getByRole('button', { name: '登录' }));

    await user.type(await screen.findByLabelText('验证码'), '000000');
    await user.click(screen.getByRole('button', { name: '验证' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('邮箱、密码或验证码不正确');
    expect(screen.getByText('请求编号：r2')).toBeInTheDocument();

    await user.clear(screen.getByLabelText('验证码'));
    await user.type(screen.getByLabelText('验证码'), '492871');
    await user.click(screen.getByRole('button', { name: '验证' }));
    expect(await screen.findByRole('heading', { name: '概览' })).toBeInTheDocument();
    expect(screen.getByText('当前管理员：ops@example.com')).toBeInTheDocument();
  });
});
