// SPDX-License-Identifier: AGPL-3.0-or-later
// 账号安全：启用与停用 TOTP、恢复码、修改密码与重新验证（AUTH-11、AUTH-23、UI-09），以及会话失效的处理。
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { fakeServer, json, me, noContent, problem, renderPortal } from '../test/harness';

const CODES = ['7KQ2-XM4P', '9RTB-3VNL', 'H6WD-2CJE', 'M8FA-5YQS', 'P3ZG-7KXR', 'T2NV-8LHD', 'W5JC-4PEB', 'X9QM-6RFT', 'Y4HK-1SGA', 'Z7LE-3UWN'];
const SECRET = 'JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP';

describe('启用 TOTP', () => {
  it('显示二维码与密钥，提交验证码后一次性展示 10 个恢复码', async () => {
    let enabled = false;
    const s = fakeServer({
      'GET /v1/me': () => json(200, me({ is_mfa_enabled: enabled, mfa_methods: enabled ? ['totp', 'recovery_code'] : [] })),
      'POST /v1/me/mfa/totp': () =>
        json(201, {
          secret: SECRET,
          otpauth_uri: `otpauth://totp/Akari:alice%40example.com?secret=${SECRET}&issuer=Akari`,
          expires_at: '2026-10-01T10:15:00+08:00',
        }),
      'POST /v1/me/mfa/totp/activation': (body) => {
        if ((body as { totp_code: string }).totp_code !== '731946') {
          return problem(400, 'invalid_request', { errors: [{ field: 'totp_code', code: 'incorrect' }] });
        }
        enabled = true;
        return json(200, { recovery_codes: CODES });
      },
    });
    renderPortal(s, '/security');
    const user = userEvent.setup();
    expect(await screen.findByText('未启用')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '启用二次验证' }));

    expect(await screen.findByRole('img', { name: '身份验证器二维码' })).toBeInTheDocument();
    const secret = screen.getByLabelText('密钥');
    expect(secret.textContent?.replace(/\s/g, '')).toBe(SECRET);
    expect(screen.getByText(/请在 .* 前完成绑定/)).toBeInTheDocument();

    await user.type(screen.getByLabelText('验证码'), '111111');
    await user.click(screen.getByRole('button', { name: '确认启用' }));
    await waitFor(() => expect(screen.getByLabelText('验证码')).toHaveAccessibleDescription('不正确'));

    await user.clear(screen.getByLabelText('验证码'));
    await user.type(screen.getByLabelText('验证码'), '731946');
    await user.click(screen.getByRole('button', { name: '确认启用' }));

    const list = await screen.findByRole('list', { name: '恢复码' });
    expect(within(list).getAllByRole('listitem').map((li) => li.textContent)).toEqual(CODES);
    expect(screen.getByRole('heading', { name: '请保存恢复码' })).toHaveFocus();
    expect(screen.getByRole('button', { name: '复制' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '下载' })).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '我已保存' }));
    expect(await screen.findByText('已启用')).toBeInTheDocument();
    expect(screen.queryByText(CODES[0]!)).not.toBeInTheDocument();
  });
});

describe('需要重新验证的操作', () => {
  it('修改密码返回 mfa_required 时弹出验证框，验证后自动重试', async () => {
    let reauthed = false;
    const s = fakeServer({
      'GET /v1/me': () => json(200, me({ is_mfa_enabled: true, mfa_methods: ['totp', 'recovery_code'] })),
      'PUT /v1/me/password': () => (reauthed ? noContent() : problem(401, 'mfa_required', { methods: ['totp', 'recovery_code'] })),
      'POST /v1/me/reauthentications': (body) => {
        if ((body as { password?: string }).password === 'wrong') {
          return problem(400, 'invalid_request', { errors: [{ field: 'password', code: 'incorrect' }] });
        }
        reauthed = true;
        return json(200, { expires_at: '2026-10-01T10:05:00+08:00' });
      },
    });
    renderPortal(s, '/security');
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText('新密码'), 'new-correct-horse');
    await user.type(screen.getByLabelText('确认新密码'), 'new-correct-horse');
    await user.click(screen.getByRole('button', { name: '修改密码' }));

    const dialog = await screen.findByRole('dialog', { name: '验证身份' });
    // 密码总是可用；另列出服务端给出的方式。
    expect(within(dialog).getAllByRole('radio').map((r) => (r as HTMLInputElement).value)).toEqual(['password', 'totp', 'recovery_code']);
    const pw = within(dialog).getByLabelText('密码', { selector: 'input[type=password]' });
    await waitFor(() => expect(pw).toHaveFocus());
    await user.type(pw, 'wrong');
    await user.click(within(dialog).getByRole('button', { name: '验证并继续' }));
    await waitFor(() => expect(pw).toHaveAccessibleDescription('不正确'));

    await user.clear(pw);
    await user.type(pw, 'correct-horse');
    await user.click(within(dialog).getByRole('button', { name: '验证并继续' }));
    expect(await screen.findByText('密码已修改。')).toBeInTheDocument();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(s.calls.filter((c) => c.path === '/v1/me/password')).toHaveLength(2);
    expect(s.called('POST', '/v1/me/reauthentications').map((c) => c.body)).toEqual([{ password: 'wrong' }, { password: 'correct-horse' }]);
  });

  it('可以改用 TOTP 验证码；取消时不显示错误', async () => {
    const s = fakeServer({
      'GET /v1/me': () => json(200, me({ is_mfa_enabled: true, mfa_methods: ['totp', 'recovery_code'] })),
      'POST /v1/me/mfa/recovery-codes': () => problem(401, 'mfa_required', { challenge_id: '0192f0c4-3a00-7000-8000-00000000e002', methods: ['totp'] }),
    });
    renderPortal(s, '/security');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '重新生成恢复码' }));
    const confirm = await screen.findByRole('dialog', { name: '重新生成恢复码？' });
    expect(confirm).toHaveTextContent('现有的 10 个恢复码将立即作废');
    await user.click(within(confirm).getByRole('button', { name: '重新生成恢复码' }));

    const dialog = await screen.findByRole('dialog', { name: '验证身份' });
    await user.click(within(dialog).getByRole('radio', { name: '验证码' }));
    await user.type(within(dialog).getByRole('textbox', { name: '验证码' }), '12');
    await user.click(within(dialog).getByRole('button', { name: '验证并继续' }));
    expect(within(dialog).getByRole('textbox', { name: '验证码' })).toHaveAccessibleDescription('请输入 6 位数字');
    await user.click(within(dialog).getByRole('button', { name: '取消' }));

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(s.called('POST', '/v1/me/reauthentications')).toHaveLength(0);
  });

  it('停用前二次确认并说明影响', async () => {
    let enabled = true;
    const s = fakeServer({
      'GET /v1/me': () => json(200, me({ is_mfa_enabled: enabled })),
      'DELETE /v1/me/mfa/totp': () => {
        enabled = false;
        return noContent();
      },
    });
    renderPortal(s, '/security');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '停用二次验证' }));
    const confirm = await screen.findByRole('dialog', { name: '停用二次验证？' });
    expect(confirm).toHaveTextContent('停用后登录只需要密码，现有的恢复码全部作废。');
    await user.click(within(confirm).getByRole('button', { name: '停用二次验证' }));
    expect(await screen.findByText('未启用')).toBeInTheDocument();
    expect(s.called('DELETE', '/v1/me/mfa/totp')).toHaveLength(1);
  });
});

describe('会话', () => {
  it('访问令牌过期时刷新后继续；刷新失败时回到登录页', async () => {
    let access = false;
    let refreshOk = true;
    const s = fakeServer({
      'GET /v1/me': () => (access ? json(200, me()) : problem(401, 'unauthenticated')),
      'POST /v1/oauth/token': () => {
        if (!refreshOk) return json(400, { error: 'invalid_grant' });
        access = true;
        return json(200, { token_type: 'Bearer', expires_in: 900 });
      },
      'PUT /v1/me/password': () => (access ? noContent() : problem(401, 'unauthenticated')),
    });
    const history = renderPortal(s, '/security');
    const user = userEvent.setup();
    // 首次加载：/v1/me 401 → 刷新 → 重试成功。
    expect(await screen.findByRole('heading', { name: '账号安全' })).toBeInTheDocument();
    expect(s.called('POST', '/v1/oauth/token')[0]!.body).toBe('grant_type=refresh_token');

    access = false;
    refreshOk = false;
    await user.type(screen.getByLabelText('新密码'), 'new-correct-horse');
    await user.type(screen.getByLabelText('确认新密码'), 'new-correct-horse');
    await user.click(screen.getByRole('button', { name: '修改密码' }));
    expect(await screen.findByRole('heading', { name: '登录' })).toBeInTheDocument();
    expect(history.location.pathname).toBe('/login');
    expect(history.location.search).toContain('redirect=%2Fsecurity');
  });
});
