// SPDX-License-Identifier: AGPL-3.0-or-later
// M1-02 端到端：在真实控制面上完成超级管理员首次登录绑定 TOTP → 创建自定义角色 → 邀请客服 →
// 被邀请人从邮件链接接受邀请并首次登录 → 客服只读（看不到管理员、角色、审计日志）→ 审计日志可查 →
// 移除最后一个超级管理员被拒绝。由 server/e2e/admin（make e2e-admin）启动，环境变量见该文件。
// 各测试按顺序共用状态（串行）；TOTP 每个时间步只能使用一次（AUTH-11），因此记录最近使用的时间步。
import { randomBytes } from 'node:crypto';
import { expect, test, type Browser, type Page } from '@playwright/test';
import { nextMessageText, seenMessages } from './mail';
import { freshTotp } from './totp';

const ADMIN = (() => {
  const u = process.env.ADMIN_URL ?? '';
  return u.endsWith('/') ? u : `${u}/`;
})();
const ADMIN_EMAIL = process.env.ADMIN_EMAIL ?? '';
const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD ?? '';

test.describe.configure({ mode: 'serial' });

// 接口的 4xx（未登录时的 /v1/staff/me、第一步登录的 mfa_required、测试构造的错误）属于预期；其余控制台错误使测试失败。
function collectPageErrors(page: Page) {
  const errors: string[] = [];
  page.on('console', (m) => {
    const expected = /status of 4\d\d/.test(m.text()) && /\/v1\//.test(m.location().url);
    if (m.type() === 'error' && !expected) errors.push(m.text());
  });
  page.on('pageerror', (e) => errors.push(String(e)));
  return errors;
}

interface Totp {
  secret: string;
  step: number;
}

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

async function newPage(browser: Browser) {
  const page = await (await browser.newContext()).newPage();
  return { page, errors: collectPageErrors(page) };
}

/** 首次登录：提交密码 → 绑定 TOTP（读取页面上的密钥）→ 保存恢复码 → 进入概览（AUTH-21）。 */
async function firstSignIn(page: Page, email: string, password: string): Promise<Totp> {
  await page.goto(`${ADMIN}login`);
  await page.getByLabel('邮箱').fill(email);
  await page.getByLabel('密码').fill(password);
  await page.getByRole('button', { name: '登录' }).click();
  await expect(page.getByRole('img', { name: '绑定身份验证器的二维码' })).toBeVisible();
  const secret = (await page.locator('code').first().textContent())!.replace(/\s/g, '');
  expect(secret).toMatch(/^[A-Z2-7]+=*$/);
  const { code, step } = await freshTotp(secret);
  await page.getByLabel('验证码').fill(code);
  await page.getByRole('button', { name: '验证', exact: true }).click();
  await expect(page.getByRole('heading', { name: '请保存恢复码' })).toBeVisible();
  await expect(page.getByRole('list', { name: '恢复码' }).getByRole('listitem')).toHaveCount(10);
  await page.getByRole('button', { name: '我已保存' }).click();
  await expect(page.getByRole('heading', { name: '概览', level: 1 })).toBeVisible();
  return { secret, step };
}

/**
 * 点击敏感操作的提交按钮后：若弹出重新验证框则用新的 TOTP 完成（AUTH-19），然后等待 done 出现。
 * 5 分钟内复用断言时不会弹出验证框。
 */
async function submitSensitive(page: Page, totp: Totp, submit: () => Promise<void>, done: () => Promise<void>) {
  await submit();
  const stepUp = page.getByRole('dialog', { name: '验证身份' });
  // 两个分支中未胜出的一方稍后会超时失败，吞掉其错误，避免未处理的拒绝。
  const outcome = await Promise.race([
    stepUp.waitFor({ state: 'visible', timeout: 60_000 }).then(
      () => 'step-up' as const,
      () => 'timeout' as const,
    ),
    done().then(
      () => 'done' as const,
      () => 'timeout' as const,
    ),
  ]);
  if (outcome === 'done') return;
  if (outcome === 'timeout') throw new Error('既没有弹出重新验证框，也没有出现预期结果');
  const { code, step } = await freshTotp(totp.secret, totp.step);
  totp.step = step;
  await stepUp.getByLabel('验证码').fill(code);
  await stepUp.getByRole('button', { name: '验证' }).click();
  await done();
}

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
