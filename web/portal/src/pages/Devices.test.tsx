// SPDX-License-Identifier: AGPL-3.0-or-later
// 设备与登录会话：列表字段、设备名额与已满提示（AUTH-14）、移除设备与移除当前设备即登出（AUTH-15）、错误状态（UI-02）。
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { fakeServer, json, me, noContent, problem, renderPortal } from '../test/harness';

const PHONE = {
  id: '0192f0c4-4a00-7000-8000-00000000f001',
  platform: 'ios',
  model: 'iPhone17,1',
  app_version: '1.4.0',
  created_at: '2026-09-01T09:20:00+08:00',
  last_seen_at: '2026-10-01T09:58:00+08:00',
  ip_prefix: '203.0.113.0/24',
  is_current: false,
  has_credential: true,
};
const BROWSER = {
  id: '0192f0c4-4a00-7000-8000-00000000f002',
  platform: 'web',
  model: 'Firefox 131 on Windows',
  app_version: null,
  created_at: '2026-10-01T09:00:00+08:00',
  last_seen_at: '2026-10-01T10:00:00+08:00',
  ip_prefix: '198.51.100.0/24',
  is_current: true,
  has_credential: false,
};
const LAPTOP = {
  id: '0192f0c4-4a00-7000-8000-00000000f003',
  platform: 'macos',
  model: null,
  app_version: '1.3.2',
  created_at: '2026-09-20T12:00:00+08:00',
  last_seen_at: null,
  ip_prefix: null,
  is_current: false,
  has_credential: false,
};

function server(items: unknown[], limit: number, routes: Parameters<typeof fakeServer>[0] = {}) {
  return fakeServer({
    'GET /v1/me': () => json(200, me()),
    'GET /v1/me/devices': () => json(200, { device_limit: limit, items }),
    ...routes,
  });
}

describe('设备列表', () => {
  it('显示平台、型号、版本、最后活跃（用户时区）、IP 段、当前设备与凭据状态', async () => {
    renderPortal(server([PHONE, BROWSER], 3), '/devices');
    const phone = await screen.findByRole('article', { name: 'iPhone17,1' });
    expect(phone).toHaveTextContent('平台iOS');
    expect(phone).toHaveTextContent('应用版本1.4.0');
    // 用户时区 Asia/Shanghai：09:58。
    expect(phone).toHaveTextContent(/最后活跃.*09:58/);
    expect(phone).toHaveTextContent('IP 段203.0.113.0/24');
    expect(phone).toHaveTextContent('代理凭据已下发');
    expect(phone).not.toHaveTextContent('当前设备');

    const browser = screen.getByRole('article', { name: /Firefox 131 on Windows/ });
    expect(within(browser).getByText('当前设备')).toBeInTheDocument();
    expect(browser).toHaveTextContent('平台浏览器');
    expect(browser).not.toHaveTextContent('应用版本');
    expect(browser).toHaveTextContent('代理凭据不需要（浏览器）');

    expect(screen.getByText('设备名额：已用 1 / 3')).toBeInTheDocument();
    expect(screen.queryByText(/设备名额已用完/)).not.toBeInTheDocument();
  });

  it('名额已满且有设备未获得凭据时提示；没有型号时以平台命名', async () => {
    renderPortal(server([PHONE, LAPTOP, BROWSER], 1), '/devices', 'en');
    const laptop = await screen.findByRole('article', { name: 'macOS' });
    expect(laptop).toHaveTextContent('Proxy credentialNot issued (slots full)');
    expect(laptop).toHaveTextContent('Last activeNo record');
    expect(screen.getByText('Device slots: 1 of 1 used')).toBeInTheDocument();
    expect(screen.getByRole('status')).toHaveTextContent('All device slots are in use');
  });

  it('加载失败时按 code 显示错误与 request_id，可以重试', async () => {
    let fail = true;
    renderPortal(
      server([], 1, { 'GET /v1/me/devices': () => (fail ? problem(429, 'rate_limited') : json(200, { device_limit: 1, items: [PHONE] })) }),
      '/devices',
    );
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('操作过于频繁');
    expect(alert).toHaveTextContent('req_rate_limited');
    fail = false;
    await userEvent.setup().click(within(alert).getByRole('button', { name: '重试' }));
    expect(await screen.findByRole('article', { name: 'iPhone17,1' })).toBeInTheDocument();
  });

  it('没有设备时显示空状态', async () => {
    renderPortal(server([], 1), '/devices');
    expect(await screen.findByText('没有登录中的设备。')).toBeInTheDocument();
  });
});

describe('移除设备', () => {
  it('确认后移除其他设备并刷新列表', async () => {
    let items = [PHONE, BROWSER];
    const s = server([], 3, {
      'GET /v1/me/devices': () => json(200, { device_limit: 3, items }),
      [`DELETE /v1/me/devices/${PHONE.id}`]: () => {
        items = [BROWSER];
        return noContent();
      },
    });
    renderPortal(s, '/devices');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '移除 iPhone17,1' }));
    const dialog = await screen.findByRole('dialog', { name: '移除 iPhone17,1？' });
    expect(dialog).toHaveTextContent('代理凭据立即吊销');
    // 取消不发请求。
    await user.click(within(dialog).getByRole('button', { name: '取消' }));
    expect(s.called('DELETE', `/v1/me/devices/${PHONE.id}`)).toHaveLength(0);

    await user.click(screen.getByRole('button', { name: '移除 iPhone17,1' }));
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '移除' }));
    expect(await screen.findByText('已移除 iPhone17,1。')).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole('article', { name: 'iPhone17,1' })).not.toBeInTheDocument());
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(s.called('DELETE', `/v1/me/devices/${PHONE.id}`)).toHaveLength(1);
  });

  it('移除当前设备等同登出：回到登录页，不再调用登出接口', async () => {
    const s = server([PHONE, BROWSER], 3, { [`DELETE /v1/me/devices/${BROWSER.id}`]: () => noContent() });
    const history = renderPortal(s, '/devices');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '移除 Firefox 131 on Windows' }));
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveTextContent('这是你正在使用的设备');
    await user.click(within(dialog).getByRole('button', { name: '移除' }));
    expect(await screen.findByRole('heading', { name: '登录' })).toBeInTheDocument();
    expect(history.location.pathname).toBe('/login');
    expect(s.called('DELETE', '/v1/sessions/current')).toHaveLength(0);
  });

  it('移除失败时显示错误与 request_id；设备已不存在时只刷新列表', async () => {
    let items = [PHONE, LAPTOP, BROWSER];
    const s = server([], 3, {
      'GET /v1/me/devices': () => json(200, { device_limit: 3, items }),
      [`DELETE /v1/me/devices/${PHONE.id}`]: () => problem(500, 'internal'),
      [`DELETE /v1/me/devices/${LAPTOP.id}`]: () => {
        items = [PHONE, BROWSER];
        return problem(404, 'not_found');
      },
    });
    renderPortal(s, '/devices');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '移除 iPhone17,1' }));
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '移除' }));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('req_internal');
    expect(screen.getByRole('article', { name: 'iPhone17,1' })).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '移除 macOS' }));
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '移除' }));
    await waitFor(() => expect(screen.queryByRole('article', { name: 'macOS' })).not.toBeInTheDocument());
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});
