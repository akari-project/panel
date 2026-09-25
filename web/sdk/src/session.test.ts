// SPDX-License-Identifier: AGPL-3.0-or-later
import { describe, expect, it, vi } from 'vitest';
import { createClientApi, createConsoleApi, sessionKeys, unwrap, type Problem } from './index';

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': status >= 400 ? 'application/problem+json' : 'application/json' },
  });
}

const unauthenticated = () => json(401, { code: 'unauthenticated', status: 401 });

/** 模拟服务端：访问令牌以一个标志表示，刷新成功后置为有效。 */
function fakeServer({ refreshOk = true, reauthRequired = false } = {}) {
  let accessValid = false;
  let reauthed = !reauthRequired;
  const calls: { method: string; path: string; body: string; contentType: string | null }[] = [];
  const fetch = vi.fn(async (input: RequestInfo | URL) => {
    const req = input as Request;
    const path = new URL(req.url).pathname;
    const body = await req.text();
    calls.push({ method: req.method, path, body, contentType: req.headers.get('content-type') });
    if (path === '/v1/oauth/token') {
      // 让并发请求有机会在刷新完成前到达。
      await new Promise((r) => setTimeout(r, 10));
      if (!refreshOk) return json(400, { error: 'invalid_grant' });
      accessValid = true;
      return json(200, { token_type: 'Bearer', expires_in: 900 });
    }
    if (path === '/v1/sessions') return json(401, { code: 'unauthenticated' });
    if (!accessValid) return unauthenticated();
    if (path === '/v1/me/password' && !reauthed) {
      return json(401, { code: 'mfa_required', methods: ['totp', 'recovery_code'] });
    }
    if (path === '/v1/me/reauthentications') {
      reauthed = true;
      return json(200, { expires_at: '2026-10-01T10:05:00+08:00' });
    }
    if (path === '/v1/me/password') return new Response(null, { status: 204 });
    return json(200, { email: 'alice@example.com' });
  });
  return { fetch, calls, setAccess: (v: boolean) => (accessValid = v) };
}

describe('访问令牌过期', () => {
  it('刷新一次后重试原请求，请求体原样重放', async () => {
    const s = fakeServer({ reauthRequired: false });
    const api = createClientApi({ baseUrl: 'https://api.example.invalid', fetch: s.fetch });
    await expect(unwrap(api.PUT('/v1/me/password', { body: { new_password: 'new-password-123' } }))).resolves.toBeUndefined();
    expect(s.calls.map((c) => c.path)).toEqual(['/v1/me/password', '/v1/oauth/token', '/v1/me/password']);
    expect(s.calls[1]).toMatchObject({ body: 'grant_type=refresh_token', contentType: 'application/x-www-form-urlencoded' });
    expect(s.calls[0]!.body).toBe(s.calls[2]!.body);
  });

  it('并发请求只刷新一次', async () => {
    const s = fakeServer();
    const api = createClientApi({ baseUrl: 'https://api.example.invalid', fetch: s.fetch });
    await Promise.all([unwrap(api.GET('/v1/me')), unwrap(api.GET('/v1/me')), unwrap(api.GET('/v1/me'))]);
    expect(s.calls.filter((c) => c.path === '/v1/oauth/token')).toHaveLength(1);
  });

  it('刷新失败时通知会话失效，并返回原来的 401', async () => {
    const s = fakeServer({ refreshOk: false });
    const api = createClientApi({ baseUrl: 'https://api.example.invalid', fetch: s.fetch });
    const expired = vi.fn();
    api.setSessionHooks({ sessionExpired: expired });
    await expect(unwrap(api.GET('/v1/me'))).rejects.toMatchObject({ problem: { code: 'unauthenticated' } });
    expect(expired).toHaveBeenCalledTimes(1);
  });

  it('登录接口的 401 不触发刷新', async () => {
    const s = fakeServer();
    const api = createClientApi({ baseUrl: 'https://api.example.invalid/prefix', fetch: s.fetch });
    await expect(
      unwrap(api.POST('/v1/sessions', { body: { email: 'a@example.com', password: 'x', device: { platform: 'web' } } })),
    ).rejects.toMatchObject({ problem: { code: 'unauthenticated' } });
    expect(s.calls.map((c) => c.path)).toEqual(['/prefix/v1/sessions']);
  });
});

describe('重新验证', () => {
  it('mfa_required 时调用 reauthenticate，完成后重试原请求', async () => {
    const s = fakeServer({ reauthRequired: true });
    s.setAccess(true);
    const api = createClientApi({ baseUrl: 'https://api.example.invalid', fetch: s.fetch });
    const reauthenticate = vi.fn(async (p: Problem) => {
      expect(p.body.methods).toEqual(['totp', 'recovery_code']);
      await unwrap(api.POST('/v1/me/reauthentications', { body: { password: 'secret' } }));
      return true;
    });
    api.setSessionHooks({ reauthenticate });
    await unwrap(api.PUT('/v1/me/password', { body: { new_password: 'new-password-123' } }));
    expect(reauthenticate).toHaveBeenCalledTimes(1);
    expect(s.calls.map((c) => c.path)).toEqual(['/v1/me/password', '/v1/me/reauthentications', '/v1/me/password']);
  });

  it('用户取消时返回原来的 mfa_required', async () => {
    const s = fakeServer({ reauthRequired: true });
    s.setAccess(true);
    const api = createClientApi({ baseUrl: 'https://api.example.invalid', fetch: s.fetch });
    const undo = api.setSessionHooks({ reauthenticate: async () => false });
    await expect(unwrap(api.PUT('/v1/me/password', { body: { new_password: 'new-password-123' } }))).rejects.toMatchObject({
      problem: { code: 'mfa_required' },
    });
    undo();
  });

  it('没有提供重新验证框时直接返回 mfa_required', async () => {
    const s = fakeServer({ reauthRequired: true });
    s.setAccess(true);
    const api = createClientApi({ baseUrl: 'https://api.example.invalid', fetch: s.fetch });
    await expect(unwrap(api.PUT('/v1/me/password', { body: { new_password: 'new-password-123' } }))).rejects.toMatchObject({
      problem: { code: 'mfa_required' },
    });
    expect(s.calls).toHaveLength(1);
  });
});

describe('管理接口', () => {
  it('跨标签页的锁与时间戳使用独立命名空间，用户中心保持原名', () => {
    expect(sessionKeys()).toEqual({ lock: 'panel.session-refresh', refreshedAt: 'panel.session-refreshed-at' });
    expect(sessionKeys('console')).toEqual({
      lock: 'panel.console.session-refresh',
      refreshedAt: 'panel.console.session-refreshed-at',
    });
  });

  it('敏感操作返回 mfa_required 时，验证后附加 Mfa-Assertion 并以同一幂等键重试（AUTH-19、CONV-12）', async () => {
    const seen: { assertion: string | null; key: string | null; body: string }[] = [];
    const fetch = vi.fn(async (input: RequestInfo | URL) => {
      const req = input as Request;
      seen.push({ assertion: req.headers.get('Mfa-Assertion'), key: req.headers.get('Idempotency-Key'), body: await req.text() });
      if (req.headers.get('Mfa-Assertion') !== 'v4.public.x') return json(401, { code: 'mfa_required', methods: ['totp'] });
      return json(201, { name: 'finance' });
    });
    const api = createConsoleApi({ baseUrl: 'https://console.example.invalid', fetch });
    const reauthenticate = vi.fn(async (p: Problem) => {
      expect(p.body.methods).toEqual(['totp']);
      return { headers: { 'Mfa-Assertion': 'v4.public.x' } };
    });
    api.setSessionHooks({ reauthenticate });
    const key = '01927c3e-8a41-7030-9d3e-5f6a7b8c0099';
    await unwrap(
      api.POST('/v1/roles', {
        params: { header: { 'Mfa-Assertion': 'v4.public.expired', 'Idempotency-Key': key } },
        body: { name: 'finance', permissions: ['orders.read'], reason: '财务' },
      }),
    );
    expect(reauthenticate).toHaveBeenCalledTimes(1);
    expect(seen).toHaveLength(2);
    expect(seen[1]).toMatchObject({ assertion: 'v4.public.x', key });
    expect(seen[1]!.body).toBe(seen[0]!.body);
  });
});
