// SPDX-License-Identifier: AGPL-3.0-or-later
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it } from 'vitest';
import { fakeServer, json, noContent, problem, renderAdmin, superadmin, support } from '../test/harness';

const staffList = {
  items: [
    {
      account_id: '01927c3e-8a41-7004-9d3e-5f6a7b8c0004',
      email: 'ops@example.com',
      roles: ['operator'],
      has_totp: true,
      has_passkey: false,
      last_login_at: null,
      created_at: '2026-03-01T10:00:00+08:00',
    },
  ],
  next_cursor: null,
};

const roles = {
  items: [
    { name: 'superadmin', description: null, permissions: ['*'], is_builtin: true, staff_count: 1, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' },
    { name: 'operator', description: null, permissions: ['plans.*'], is_builtin: true, staff_count: 1, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' },
    { name: 'finance', description: '财务', permissions: ['orders.read'], is_builtin: false, staff_count: 2, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' },
  ],
};

const invitation = {
  id: '01927c3e-8a41-7022-9d3e-5f6a7b8c0022',
  email: 'new@example.com',
  roles: ['operator'],
  status: 'pending',
  invited_by: superadmin.account_id,
  expires_at: '2026-09-28T10:00:00+08:00',
  created_at: '2026-09-25T10:00:00+08:00',
};

const assertion = (token = 'v4.public.assert') =>
  json(201, { mfa_assertion: token, expires_at: new Date(Date.now() + 300_000).toISOString() });

afterEach(() => {
  window.location.hash = '';
});

describe('按权限显示菜单与页面（AUTH-17）', () => {
  it('superadmin 看到管理员、角色、审计日志', async () => {
    renderAdmin(fakeServer(superadmin), '/');
    const nav = await screen.findByRole('navigation', { name: '主导航' });
    for (const name of ['管理员', '角色', '审计日志']) {
      expect(within(nav).getByRole('link', { name })).toBeInTheDocument();
    }
  });

  it('客服看不到这些菜单；直接访问显示无权限且不请求接口', async () => {
    const server = fakeServer(support);
    renderAdmin(server, '/roles');
    expect(await screen.findByText('没有权限执行此操作。')).toBeInTheDocument();
    const nav = screen.getByRole('navigation', { name: '主导航' });
    expect(within(nav).queryByRole('link', { name: '角色' })).not.toBeInTheDocument();
    expect(within(nav).queryByRole('link', { name: '审计日志' })).not.toBeInTheDocument();
    expect(server.called('GET', '/v1/roles')).toHaveLength(0);
  });
});

describe('敏感操作（AUTH-19）', () => {
  it('邀请：先重新验证，带 Mfa-Assertion、Idempotency-Key 与原因；之后的敏感操作复用断言', async () => {
    let stepUps = 0;
    const server = fakeServer(superadmin, {
      'GET /v1/staff': () => json(200, staffList),
      'GET /v1/roles': () => json(200, roles),
      'GET /v1/staff-invitations': () => json(200, { items: [invitation], next_cursor: null }),
      'POST /v1/staff/me/step-up': (body) => {
        stepUps++;
        return (body as { totp_code: string }).totp_code === '000000'
          ? problem(400, 'invalid_request', { errors: [{ field: 'totp_code', code: 'incorrect' }] })
          : assertion();
      },
      'POST /v1/staff-invitations': () => json(201, invitation),
      'DELETE /v1/staff-invitations/01927c3e-8a41-7022-9d3e-5f6a7b8c0022': () => noContent(),
    });
    renderAdmin(server, '/staff');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '邀请管理员' }));
    const dialog = await screen.findByRole('dialog', { name: '邀请管理员' });
    await user.type(within(dialog).getByLabelText('邮箱'), 'new@example.com');
    await user.click(await within(dialog).findByRole('checkbox', { name: '运营' }));
    await user.type(within(dialog).getByLabelText('操作原因'), '新同事');
    await user.click(within(dialog).getByRole('button', { name: '发送邀请' }));

    const stepUp = await screen.findByRole('dialog', { name: '验证身份' });
    await user.type(within(stepUp).getByLabelText('验证码'), '000000');
    await user.click(within(stepUp).getByRole('button', { name: '验证' }));
    expect(await within(stepUp).findByText('不正确')).toBeInTheDocument();
    await user.clear(within(stepUp).getByLabelText('验证码'));
    await user.type(within(stepUp).getByLabelText('验证码'), '492871');
    await user.click(within(stepUp).getByRole('button', { name: '验证' }));

    expect(await screen.findByText('已向 new@example.com 发送邀请。')).toBeInTheDocument();
    const [post] = server.called('POST', '/v1/staff-invitations');
    expect(post!.body).toEqual({ email: 'new@example.com', roles: ['operator'], reason: '新同事' });
    expect(post!.headers.get('Mfa-Assertion')).toBe('v4.public.assert');
    expect(post!.headers.get('Idempotency-Key')).toMatch(/^[0-9a-f-]{36}$/);

    // 撤销邀请：DELETE 的原因在 Audit-Reason 请求头中（UTF-8 百分号编码），断言仍在有效期内，不再验证。
    await user.click(screen.getByRole('link', { name: '邀请' }));
    await user.click(await screen.findByRole('button', { name: '撤销对 new@example.com 的邀请' }));
    const confirm = await screen.findByRole('dialog', { name: '撤销邀请？' });
    await user.type(within(confirm).getByLabelText('操作原因'), '邮箱错了 100%');
    await user.click(within(confirm).getByRole('button', { name: '撤销' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    const [del] = server.called('DELETE', `/v1/staff-invitations/${invitation.id}`);
    expect(del!.headers.get('Audit-Reason')).toBe(encodeURIComponent('邮箱错了 100%'));
    expect(del!.headers.get('Mfa-Assertion')).toBe('v4.public.assert');
    expect(stepUps).toBe(2);
  });

  it('断言失效时服务端返回 mfa_required：重新验证后以新断言和同一幂等键重试（CONV-12）', async () => {
    let issued = 0;
    const server = fakeServer(superadmin, {
      'GET /v1/staff': () => json(200, staffList),
      'GET /v1/roles': () => json(200, roles),
      'POST /v1/staff/me/step-up': () => assertion(`v4.public.a${++issued}`),
      'POST /v1/staff-invitations': (_b, req) =>
        req.headers.get('Mfa-Assertion') === 'v4.public.a2' ? json(201, invitation) : problem(401, 'mfa_required', { methods: ['totp'] }),
    });
    renderAdmin(server, '/staff');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '邀请管理员' }));
    const dialog = await screen.findByRole('dialog', { name: '邀请管理员' });
    await user.type(within(dialog).getByLabelText('邮箱'), 'new@example.com');
    await user.click(await within(dialog).findByRole('checkbox', { name: '运营' }));
    await user.type(within(dialog).getByLabelText('操作原因'), '新同事');
    await user.click(within(dialog).getByRole('button', { name: '发送邀请' }));
    for (let i = 1; i <= 2; i++) {
      const stepUp = await screen.findByRole('dialog', { name: '验证身份' });
      // 第二次打开的验证框不保留上次的输入。
      await waitFor(() => expect(within(stepUp).getByLabelText('验证码')).toHaveValue(''));
      await user.type(within(stepUp).getByLabelText('验证码'), '492871');
      await user.click(within(stepUp).getByRole('button', { name: '验证' }));
      await waitFor(() => expect(issued).toBe(i));
    }
    expect(await screen.findByText('已向 new@example.com 发送邀请。')).toBeInTheDocument();
    const posts = server.called('POST', '/v1/staff-invitations');
    expect(posts.map((p) => p.headers.get('Mfa-Assertion'))).toEqual(['v4.public.a1', 'v4.public.a2']);
    expect(posts[1]!.headers.get('Idempotency-Key')).toBe(posts[0]!.headers.get('Idempotency-Key'));
  });

  it('取消重新验证时不发送请求，表单保留', async () => {
    const server = fakeServer(superadmin, {
      'GET /v1/staff': () => json(200, staffList),
      'GET /v1/roles': () => json(200, roles),
    });
    renderAdmin(server, '/staff');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '移除 ops@example.com' }));
    const confirm = await screen.findByRole('dialog', { name: '移除管理员？' });
    await user.type(within(confirm).getByLabelText('操作原因'), '离职');
    await user.click(within(confirm).getByRole('button', { name: '移除' }));
    const stepUp = await screen.findByRole('dialog', { name: '验证身份' });
    await user.click(within(stepUp).getByRole('button', { name: '取消' }));
    expect(await screen.findByRole('dialog', { name: '移除管理员？' })).toBeInTheDocument();
    expect(server.called('DELETE', `/v1/staff/${staffList.items[0]!.account_id}`)).toHaveLength(0);
  });
});

describe('角色（AUTH-22）', () => {
  it('内置角色只读；权限选项不含 * 与 staff.*；修改携带 If-Match，版本冲突时提示', async () => {
    const server = fakeServer(superadmin, {
      'GET /v1/roles': () => json(200, roles),
      'GET /v1/roles/finance': () => json(200, roles.items[2], { ETag: '"v7"' }),
      'POST /v1/staff/me/step-up': () => assertion(),
      'PATCH /v1/roles/finance': () => problem(409, 'conflict'),
    });
    renderAdmin(server, '/roles');
    const user = userEvent.setup();
    expect(await screen.findByText('超级管理员')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '编辑角色 operator' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '编辑角色 finance' }));
    const dialog = await screen.findByRole('dialog', { name: '编辑角色 finance' });
    const boxes = within(dialog).getAllByRole('checkbox');
    expect(boxes).toHaveLength(16);
    expect(within(dialog).queryByText('staff.*')).not.toBeInTheDocument();
    expect(within(dialog).getByText('将影响 2 名管理员，他们的登录会话将被吊销。')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('checkbox', { name: /audit\.read/ }));
    await user.type(within(dialog).getByLabelText('操作原因'), '需要查审计');
    await user.click(within(dialog).getByRole('button', { name: '保存' }));
    const stepUp = await screen.findByRole('dialog', { name: '验证身份' });
    await user.type(within(stepUp).getByLabelText('验证码'), '492871');
    await user.click(within(stepUp).getByRole('button', { name: '验证' }));
    expect(await within(dialog).findByText('数据已被修改，请刷新后重试。')).toBeInTheDocument();
    const [patch] = server.called('PATCH', '/v1/roles/finance');
    expect(patch!.headers.get('If-Match')).toBe('"v7"');
    expect(patch!.body).toEqual({ description: '财务', permissions: ['orders.read', 'audit.read'], reason: '需要查审计' });
  });
});

describe('审计日志（AUTH-18）', () => {
  it('筛选条件写入查询参数；已知动作本地化，未知动作显示原始标识', async () => {
    const server = fakeServer(superadmin, {
      'GET /v1/audit-logs': (_b, req) =>
        json(200, {
          items: [
            {
              id: 'a1',
              actor_id: superadmin.account_id,
              actor_email: 'root@example.com',
              action: new URL(req.url).searchParams.get('action') ?? 'role.update',
              target_type: 'role',
              target_id: 'finance',
              diff: { permissions: [['orders.read'], ['orders.read', 'audit.read']] },
              reason: '需要查审计',
              ip_prefix: '198.51.100.0/24',
              request_id: 'req1',
              created_at: '2026-09-25T10:00:00+08:00',
            },
          ],
          next_cursor: null,
        }),
    });
    renderAdmin(server, '/audit-logs');
    const user = userEvent.setup();
    expect(await screen.findByText('修改角色')).toBeInTheDocument();

    await user.type(screen.getByLabelText('操作者 ID'), 'not-a-uuid');
    await user.click(screen.getByRole('button', { name: '查询' }));
    expect(await screen.findByText('请输入有效的 UUID')).toBeInTheDocument();

    await user.clear(screen.getByLabelText('操作者 ID'));
    await user.type(screen.getByLabelText('动作'), 'plan.update');
    await user.click(screen.getByRole('button', { name: '查询' }));
    const table = await screen.findByRole('table', { name: '审计日志' });
    expect(await within(table).findByText('plan.update')).toBeInTheDocument();
    expect(server.calls.at(-1)!.search).toBe('?action=plan.update');

    await user.click(within(table).getByRole('button', { name: '查看审计记录 a1' }));
    const detail = await screen.findByRole('dialog', { name: '审计记录' });
    expect(within(detail).getByText('198.51.100.0/24')).toBeInTheDocument();
    await user.click(within(detail).getByRole('button', { name: '关闭' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });
});

describe('接受邀请（AUTH-22）', () => {
  it('令牌从片段读取；新账号按服务端要求设置密码后加入', async () => {
    window.location.hash = '#token=inv_abc';
    const server = fakeServer(null, {
      'POST /v1/staff-invitations/acceptance': (body) =>
        (body as { password?: string }).password
          ? json(200, { ...staffList.items[0], email: 'new@example.com' })
          : problem(400, 'invalid_request', { errors: [{ field: 'password', code: 'required' }] }),
    });
    renderAdmin(server, '/accept-invitation#token=inv_abc');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '接受邀请' }));
    expect(await screen.findByText('该邮箱还没有账号，请设置密码。')).toBeInTheDocument();
    await user.type(screen.getByLabelText('设置密码'), 'correct-horse-battery');
    await user.click(screen.getByRole('button', { name: '接受邀请' }));
    expect(await screen.findByRole('status')).toHaveTextContent('已加入管理后台');
    const posts = server.called('POST', '/v1/staff-invitations/acceptance');
    expect(posts.map((p) => p.body)).toEqual([{ token: 'inv_abc' }, { token: 'inv_abc', password: 'correct-horse-battery' }]);
  });

  it('令牌过期与状态不允许使用专门文案', async () => {
    const server = fakeServer(null, {
      'POST /v1/staff-invitations/acceptance': () => problem(400, 'invalid_request', { errors: [{ field: 'token', code: 'expired' }] }),
    });
    renderAdmin(server, '/accept-invitation#token=inv_old');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: '接受邀请' }));
    expect(await screen.findByText('邀请链接已过期，请联系管理员重新邀请。')).toBeInTheDocument();
  });

  it('没有令牌时提示重新打开链接', async () => {
    renderAdmin(fakeServer(null), '/accept-invitation');
    expect(await screen.findByText('邀请链接不完整，请从邀请邮件中重新打开。')).toBeInTheDocument();
  });
});
