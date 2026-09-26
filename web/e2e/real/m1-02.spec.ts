// SPDX-License-Identifier: AGPL-3.0-or-later
// M1-02 端到端：在真实控制面上完成超级管理员首次登录绑定 TOTP → 创建自定义角色 → 邀请客服 →
// 被邀请人从邮件链接接受邀请并首次登录 → 客服只读（看不到管理员、角色、审计日志）→ 审计日志可查 →
// 移除最后一个超级管理员被拒绝。由 server/e2e/admin（make e2e-admin）启动，环境变量见该文件。
// 各测试按顺序共用状态（串行）；TOTP 每个时间步只能使用一次（AUTH-11），因此记录最近使用的时间步。
import { randomBytes } from 'node:crypto';
import { expect, test, type Page } from '@playwright/test';
import { ADMIN, ADMIN_EMAIL, ADMIN_PASSWORD, firstSignIn, newPage, submitSensitive, type Totp } from './admin';
import { nextMessageText, seenMessages } from './mail';

test.describe.configure({ mode: 'serial' });

const state: {
  admin?: Totp;
  adminPage?: Page;
  adminErrors?: string[];
  inviteeEmail: string;
  inviteePassword: string;
  invitee?: Totp;
} = {
  inviteeEmail: `e2e-staff-${randomBytes(6).toString('hex')}@example.com`,
  inviteePassword: 'invitee-correct-horse',
};

test('超级管理员首次登录绑定 TOTP，看到全部菜单（AUTH-21、AUTH-17）', async ({ browser }) => {
  const { page, errors } = await newPage(browser);
  state.adminPage = page;
  state.adminErrors = errors;
  state.admin = await firstSignIn(page, ADMIN_EMAIL, ADMIN_PASSWORD);
  const nav = page.getByRole('navigation', { name: '主导航' });
  for (const name of ['管理员', '角色', '审计日志']) await expect(nav.getByRole('link', { name })).toBeVisible();
  expect(errors).toEqual([]);
});

test('创建自定义角色：原因、重新验证（AUTH-19、AUTH-22）', async () => {
  const page = state.adminPage!;
  await page.getByRole('navigation', { name: '主导航' }).getByRole('link', { name: '角色' }).click();
  await page.getByRole('button', { name: '创建角色' }).click();
  const dialog = page.getByRole('dialog', { name: '创建角色' });
  await dialog.getByLabel('名称').fill('e2e-finance');
  await dialog.getByLabel('说明').fill('端到端测试：财务');
  await dialog.getByRole('checkbox', { name: /orders\.read/ }).check();
  await dialog.getByRole('checkbox', { name: /orders\.refund/ }).check();
  await expect(dialog.getByText('staff.*')).toHaveCount(0);
  await dialog.getByLabel('操作原因').fill('端到端测试');
  await submitSensitive(
    page,
    state.admin!,
    () => dialog.getByRole('button', { name: '创建', exact: true }).click(),
    () => expect(page.getByRole('button', { name: '编辑角色 e2e-finance' })).toBeVisible({ timeout: 60_000 }),
  );
  await expect(page.getByRole('dialog')).toHaveCount(0);
});

test('邀请客服，被邀请人从邮件链接接受并首次登录；客服只读（AUTH-22、AUTH-17）', async ({ browser }) => {
  const page = state.adminPage!;
  const seen = await seenMessages(state.inviteeEmail);
  await page.getByRole('navigation', { name: '主导航' }).getByRole('link', { name: '管理员' }).click();
  await page.getByRole('button', { name: '邀请管理员' }).click();
  const dialog = page.getByRole('dialog', { name: '邀请管理员' });
  await dialog.getByLabel('邮箱').fill(state.inviteeEmail);
  await dialog.getByRole('checkbox', { name: '客服' }).check();
  await dialog.getByLabel('操作原因').fill('端到端测试：新客服');
  await submitSensitive(
    page,
    state.admin!,
    () => dialog.getByRole('button', { name: '发送邀请' }).click(),
    () => expect(page.getByRole('status')).toHaveText(`已向 ${state.inviteeEmail} 发送邀请。`, { timeout: 60_000 }),
  );

  const text = await nextMessageText(state.inviteeEmail, seen);
  const m = /(https?:\/\/\S+?accept-invitation#token=[A-Za-z0-9_-]+)/.exec(text);
  expect(m, `邀请邮件中没有链接：\n${text}`).toBeTruthy();
  const link = new URL(m![1]!);
  expect(link.href.startsWith(`${ADMIN}accept-invitation#token=`)).toBe(true);

  const invitee = await newPage(browser);
  await invitee.page.goto(link.href);
  // 令牌读取后从地址栏移除。
  await expect(invitee.page).toHaveURL(`${ADMIN}accept-invitation`);
  await invitee.page.getByRole('button', { name: '接受邀请' }).click();
  await expect(invitee.page.getByLabel('设置密码')).toHaveAccessibleDescription(/该邮箱还没有账号，请设置密码。/);
  await invitee.page.getByLabel('设置密码').fill(state.inviteePassword);
  await invitee.page.getByRole('button', { name: '接受邀请' }).click();
  await expect(invitee.page.getByRole('status')).toContainText('已加入管理后台');

  // 令牌只能使用一次（重新打开链接：先离开本页，确保整页加载）。
  await invitee.page.goto('about:blank');
  await invitee.page.goto(link.href);
  await invitee.page.getByRole('button', { name: '接受邀请' }).click();
  await expect(invitee.page.getByRole('alert')).toContainText('邀请链接无效');

  state.invitee = await firstSignIn(invitee.page, state.inviteeEmail, state.inviteePassword);
  const nav = invitee.page.getByRole('navigation', { name: '主导航' });
  await expect(nav.getByRole('link')).toHaveCount(1);
  await invitee.page.goto(`${ADMIN}audit-logs`);
  await expect(invitee.page.getByRole('alert')).toHaveText('没有权限执行此操作。');
  await invitee.page.goto(`${ADMIN}staff`);
  await expect(invitee.page.getByRole('alert')).toHaveText('没有权限执行此操作。');
  expect(invitee.errors).toEqual([]);

  // 管理员列表中出现新客服。
  await page.reload();
  await expect(page.getByRole('cell', { name: state.inviteeEmail, exact: true })).toBeVisible();
});

test('审计日志记录上述操作，详情显示差异（AUTH-18、CON-09）', async () => {
  const page = state.adminPage!;
  await page.getByRole('navigation', { name: '主导航' }).getByRole('link', { name: '审计日志' }).click();
  const table = page.getByRole('table', { name: '审计日志' });
  for (const label of ['创建角色', '邀请管理员', '加入管理员']) {
    await expect(table.getByText(label, { exact: true }).first()).toBeVisible();
  }

  await page.getByLabel('动作').fill('role.create');
  await page.getByRole('button', { name: '查询' }).click();
  await expect(page).toHaveURL(/action=role\.create/);
  await expect(table.getByRole('row')).toHaveCount(2);
  await table.getByRole('button', { name: /^查看审计记录 / }).click();
  const detail = page.getByRole('dialog', { name: '审计记录' });
  await expect(detail.getByText('端到端测试', { exact: true })).toBeVisible();
  await expect(detail).toContainText('e2e-finance');
  await expect(detail.getByRole('table', { name: '变更' })).toContainText('orders.refund');
  await detail.getByRole('button', { name: '关闭' }).click();
});

test('移除最后一个超级管理员被拒绝（AUTH-22）', async () => {
  const page = state.adminPage!;
  await page.getByRole('navigation', { name: '主导航' }).getByRole('link', { name: '管理员' }).click();
  await page.getByRole('button', { name: `移除 ${ADMIN_EMAIL}` }).click();
  const confirm = page.getByRole('dialog', { name: '移除管理员？' });
  await confirm.getByLabel('操作原因').fill('端到端测试：应被拒绝');
  await submitSensitive(
    page,
    state.admin!,
    () => confirm.getByRole('button', { name: '移除' }).click(),
    () => expect(confirm.getByRole('alert')).toContainText('当前状态不允许此操作。', { timeout: 60_000 }),
  );
  await confirm.getByRole('button', { name: '取消' }).click();
  await expect(page.getByRole('cell', { name: `${ADMIN_EMAIL}（你）`, exact: true })).toBeVisible();
  expect(state.adminErrors).toEqual([]);
});
