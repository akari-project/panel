// SPDX-License-Identifier: AGPL-3.0-or-later
// 注册、邮箱验证、找回密码与重置密码页面的主要状态。
import { act, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { fakeServer, json, me, noContent, problem, renderPortal } from '../test/harness';
import { readResetToken } from './ResetPassword';

const TOKEN = 'q3v8fJb1l0m9Kp3yVZ4aQeX2nR6tUoHcFgWjD5sLkP0';

afterEach(() => {
  vi.useRealTimers();
  window.history.replaceState(null, '', '/');
});

describe('注册', () => {
  it('提交后跳到验证页并带上邮箱，附带语言与时区', async () => {
    const s = fakeServer({ 'POST /v1/accounts': () => json(202, { status: 'accepted' }) });
    const history = renderPortal(s, '/register');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('邮箱'), 'bob@example.com');
    await user.type(screen.getByLabelText('密码'), 'correct-horse');
    await user.click(screen.getByRole('button', { name: '注册' }));
    expect(await screen.findByRole('heading', { name: '验证邮箱' })).toBeInTheDocument();
    expect(history.location.pathname).toBe('/verify-email');
    expect(screen.getByText('验证码已发送到 bob@example.com，15 分钟内有效。')).toBeInTheDocument();
    const body = s.called('POST', '/v1/accounts')[0]!.body as Record<string, unknown>;
    expect(body).toMatchObject({ email: 'bob@example.com', password: 'correct-horse', locale: 'zh-CN' });
    expect(body).not.toHaveProperty('invite_code');
    expect(typeof body.timezone).toBe('string');
    // 刚发出验证邮件，重新发送按钮在倒计时中。
    expect(screen.getByRole('button', { name: /重新发送（\d+ 秒）/ })).toBeDisabled();
  });

  it('密码过短时在本地提示并与字段关联', async () => {
    const s = fakeServer({});
    renderPortal(s, '/register');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('邮箱'), 'bob@example.com');
    await user.type(screen.getByLabelText('密码'), 'short');
    await user.click(screen.getByRole('button', { name: '注册' }));
    expect(await screen.findByLabelText('密码')).toHaveAccessibleDescription('8–128 个字符 密码长度为 8–128 个字符');
    expect(s.called('POST', '/v1/accounts')).toHaveLength(0);
  });

  it('服务端的字段错误关联到对应字段', async () => {
    const s = fakeServer({
      'POST /v1/accounts': () =>
        problem(400, 'invalid_request', { errors: [{ field: 'email', code: 'not_allowed' }, { field: 'invite_code', code: 'invalid_code' }] }),
    });
    renderPortal(s, '/register');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('邮箱'), 'bob@blocked.example');
    await user.type(screen.getByLabelText('密码'), 'correct-horse');
    await user.type(screen.getByLabelText('邀请码（可选）'), 'NOPE');
    await user.click(screen.getByRole('button', { name: '注册' }));
    await waitFor(() => expect(screen.getByLabelText('邮箱')).toHaveAttribute('aria-invalid', 'true'));
    expect(screen.getByLabelText('邮箱')).toHaveAccessibleDescription('不被允许');
    expect(screen.getByLabelText('邀请码（可选）')).toHaveAccessibleDescription('站点要求邀请码时必须填写。 无效');
    expect((s.called('POST', '/v1/accounts')[0]!.body as { invite_code: string }).invite_code).toBe('NOPE');
  });

  it('注册关闭时显示 registration_closed 的文案与请求编号', async () => {
    const s = fakeServer({ 'POST /v1/accounts': () => problem(403, 'registration_closed') });
    renderPortal(s, '/register');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('邮箱'), 'bob@example.com');
    await user.type(screen.getByLabelText('密码'), 'correct-horse');
    await user.click(screen.getByRole('button', { name: '注册' }));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('当前不开放注册，或需要填写有效的邀请码。');
    expect(alert).toHaveTextContent('请求编号：req_registration_closed');
  });

  it('英文文案', async () => {
    renderPortal(fakeServer({}), '/register', 'en');
    expect(await screen.findByRole('heading', { name: 'Create an account' })).toBeInTheDocument();
    expect(screen.getByLabelText('Invite code (optional)')).toBeInTheDocument();
  });
});

describe('邮箱验证', () => {
  it('未登录：提交邮箱与验证码；验证码无效时提示在字段上', async () => {
    let attempts = 0;
    const s = fakeServer({
      'POST /v1/accounts/verification': () =>
        ++attempts === 1 ? problem(400, 'invalid_request', { errors: [{ field: 'code', code: 'invalid_code' }] }) : noContent(),
    });
    renderPortal(s, '/verify-email?email=bob%40example.com');
    const user = userEvent.setup();
    const code = await screen.findByLabelText('验证码');
    expect(screen.getByLabelText('邮箱')).toHaveValue('bob@example.com');
    await user.type(code, '000000');
    await user.click(screen.getByRole('button', { name: '验证' }));
    await waitFor(() => expect(screen.getByLabelText('验证码')).toHaveAccessibleDescription('无效'));
    await user.clear(screen.getByLabelText('验证码'));
    await user.type(screen.getByLabelText('验证码'), '482913');
    await user.click(screen.getByRole('button', { name: '验证' }));
    expect(await screen.findByText('邮箱已验证。')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '去登录' })).toBeInTheDocument();
    expect(s.called('POST', '/v1/accounts/verification')[1]!.body).toEqual({ email: 'bob@example.com', code: '482913' });
  });

  it('已登录：只提交验证码', async () => {
    const s = fakeServer({
      'GET /v1/me': () => json(200, me({ is_email_verified: false })),
      'POST /v1/accounts/verification': () => noContent(),
    });
    renderPortal(s, '/verify-email');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('验证码'), '482913');
    expect(screen.queryByLabelText('邮箱')).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '验证' }));
    expect(await screen.findByText('邮箱已验证。')).toBeInTheDocument();
    expect(s.called('POST', '/v1/accounts/verification')[0]!.body).toEqual({ code: '482913' });
  });

  it('已验证的账号直接提示', async () => {
    const s = fakeServer({ 'GET /v1/me': () => json(200, me()) });
    renderPortal(s, '/verify-email');
    expect(await screen.findByText('你的邮箱已经验证过了。')).toBeInTheDocument();
  });

  it('重新发送后按间隔倒计时；429 时按 Retry-After 倒计时', async () => {
    let n = 0;
    const s = fakeServer({
      'POST /v1/accounts/verification/resend': () =>
        ++n === 1 ? json(202, { status: 'accepted' }) : problem(429, 'rate_limited', {}, { 'Retry-After': '120' }),
    });
    renderPortal(s, '/verify-email');
    const user = userEvent.setup();
    // 未登录时需要先填写邮箱。
    await user.click(await screen.findByRole('button', { name: '重新发送验证码' }));
    expect(await screen.findByLabelText('邮箱')).toHaveAccessibleDescription('请输入有效的邮箱地址');
    expect(s.called('POST', '/v1/accounts/verification/resend')).toHaveLength(0);

    await user.type(screen.getByLabelText('邮箱'), 'bob@example.com');
    await user.click(screen.getByRole('button', { name: '重新发送验证码' }));
    expect(await screen.findByText('新的验证码已发送，之前的验证码已失效。')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /重新发送（(60|59) 秒）/ })).toBeDisabled();
    expect(s.called('POST', '/v1/accounts/verification/resend')[0]!.body).toEqual({ email: 'bob@example.com' });
  });

  it('429 时按 Retry-After 倒计时，结束后可以再次发送', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const s = fakeServer({
      'POST /v1/accounts/verification/resend': () => problem(429, 'rate_limited', {}, { 'Retry-After': '3' }),
    });
    renderPortal(s, '/verify-email');
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    await user.type(await screen.findByLabelText('邮箱'), 'bob@example.com');
    await user.click(screen.getByRole('button', { name: '重新发送验证码' }));
    expect(await screen.findByRole('button', { name: /重新发送（3 秒）/ })).toBeDisabled();
    expect(screen.getByRole('alert')).toHaveTextContent('操作过于频繁');
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3500);
    });
    expect(screen.getByRole('button', { name: '重新发送验证码' })).toBeEnabled();
  });
});

describe('找回密码', () => {
  it('提交后显示统一的提示', async () => {
    const s = fakeServer({ 'POST /v1/password-resets': () => json(202, { status: 'accepted' }) });
    renderPortal(s, '/forgot-password');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('邮箱'), 'bob@example.com');
    await user.click(screen.getByRole('button', { name: '发送重置链接' }));
    expect(await screen.findByRole('status')).toHaveTextContent('如果 bob@example.com 已注册，重置链接已发送到该邮箱');
    expect(s.called('POST', '/v1/password-resets')[0]!.body).toEqual({ email: 'bob@example.com' });
  });
});

describe('重置密码', () => {
  it('readResetToken 只接受 43 个 base64url 字符', () => {
    expect(readResetToken(`#token=${TOKEN}`)).toBe(TOKEN);
    expect(readResetToken('#token=abc')).toBeNull();
    expect(readResetToken('')).toBeNull();
  });

  it('从 URL 片段读取令牌并立即清除，提交新密码', async () => {
    window.history.replaceState(null, '', `/reset-password#token=${TOKEN}`);
    const s = fakeServer({ 'POST /v1/password-resets/confirmation': () => noContent() });
    renderPortal(s, '/reset-password');
    const user = userEvent.setup();
    const pw = await screen.findByLabelText('新密码');
    expect(window.location.hash).toBe('');
    await user.type(pw, 'new-correct-horse');
    await user.type(screen.getByLabelText('确认新密码'), 'new-correct-horse');
    await user.click(screen.getByRole('button', { name: '重置密码' }));
    expect(await screen.findByText(/密码已重置/)).toBeInTheDocument();
    expect(s.called('POST', '/v1/password-resets/confirmation')[0]!.body).toEqual({ token: TOKEN, new_password: 'new-correct-horse' });
  });

  it('两次输入不一致时提示', async () => {
    window.history.replaceState(null, '', `/reset-password#token=${TOKEN}`);
    const s = fakeServer({});
    renderPortal(s, '/reset-password');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('新密码'), 'new-correct-horse');
    await user.type(screen.getByLabelText('确认新密码'), 'other-password');
    await user.click(screen.getByRole('button', { name: '重置密码' }));
    expect(await screen.findByLabelText('确认新密码')).toHaveAccessibleDescription('两次输入的密码不一致');
  });

  it('链接过期时提示重新获取', async () => {
    window.history.replaceState(null, '', `/reset-password#token=${TOKEN}`);
    const s = fakeServer({
      'POST /v1/password-resets/confirmation': () => problem(400, 'invalid_request', { errors: [{ field: 'token', code: 'expired' }] }),
    });
    renderPortal(s, '/reset-password');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('新密码'), 'new-correct-horse');
    await user.type(screen.getByLabelText('确认新密码'), 'new-correct-horse');
    await user.click(screen.getByRole('button', { name: '重置密码' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('重置链接已过期。');
    expect(screen.getByRole('link', { name: '重新获取重置链接' })).toBeInTheDocument();
  });

  it('没有令牌时显示链接无效', async () => {
    renderPortal(fakeServer({}), '/reset-password');
    expect(await screen.findByRole('alert')).toHaveTextContent('重置链接无效或已被使用。');
  });
});

describe('登录页', () => {
  it('有注册与忘记密码的链接', async () => {
    renderPortal(fakeServer({}), '/login');
    expect(await screen.findByRole('link', { name: '忘记密码？' })).toHaveAttribute('href', '/forgot-password');
    expect(screen.getByRole('link', { name: '注册新账号' })).toHaveAttribute('href', '/register');
  });
});
