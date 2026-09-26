// SPDX-License-Identifier: AGPL-3.0-or-later
// M1-04 端到端：在真实控制面上完成超级管理员首次登录 → 创建线路组 → 创建套餐（流量以 GiB 输入）→ 关联线路组 →
// 新增价格 → 上架（影响确认）→ 停售最后一个在售价格被拒绝 → 移除线路组（影响确认、原因、重新验证）并重新关联 →
// 用户中心注册并看到该套餐。由 server/e2e/admin 启动：PANEL_E2E_ADMIN_SPEC=m1-04 make e2e-admin，环境变量见该文件。
// 各测试按顺序共用状态（串行）。
import { randomBytes } from 'node:crypto';
import { expect, test, type Page } from '@playwright/test';
import { ADMIN_EMAIL, ADMIN_PASSWORD, collectPageErrors, firstSignIn, newPage, submitSensitive, type Totp } from './admin';
import { nextMessageText, seenMessages, verificationCode } from './mail';

const PORTAL = (() => {
  const u = process.env.PORTAL_URL ?? '';
  return u.endsWith('/') ? u : `${u}/`;
})();

test.describe.configure({ mode: 'serial' });

const suffix = randomBytes(3).toString('hex');
const state: { admin?: Totp; page?: Page; errors?: string[]; planUrl?: string } = {};
const GROUP = `e2e 亚太 ${suffix}`;
const PLAN = `e2e 标准版 ${suffix}`;

test('超级管理员登录，看到套餐与线路组菜单（AUTH-17）', async ({ browser }) => {
  const { page, errors } = await newPage(browser);
  state.page = page;
  state.errors = errors;
  state.admin = await firstSignIn(page, ADMIN_EMAIL, ADMIN_PASSWORD);
  const nav = page.getByRole('navigation', { name: '主导航' });
  for (const name of ['套餐与价格', '线路组']) await expect(nav.getByRole('link', { name })).toBeVisible();
});

test('创建线路组', async () => {
  const page = state.page!;
  await page.getByRole('navigation', { name: '主导航' }).getByRole('link', { name: '线路组' }).click();
  await page.getByRole('button', { name: '新建线路组' }).click();
  const dialog = page.getByRole('dialog', { name: '新建线路组' });
  await dialog.getByLabel('名称').fill(GROUP);
  await dialog.getByLabel('最低等级').fill('1');
  await dialog.getByRole('button', { name: '创建' }).click();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await expect(page.getByRole('cell', { name: GROUP, exact: true })).toBeVisible();
});

test('创建套餐，流量按 1024 进位保存（CONV-33、BIL-26）', async () => {
  const page = state.page!;
  await page.getByRole('navigation', { name: '主导航' }).getByRole('link', { name: '套餐与价格' }).click();
  await page.getByRole('button', { name: '新建套餐' }).click();
  const dialog = page.getByRole('dialog', { name: '新建套餐' });
  await dialog.getByLabel('名称').fill(PLAN);
  await dialog.getByLabel('等级').fill('1');
  await dialog.getByLabel('每周期流量').fill('100');
  await dialog.getByLabel('设备上限').fill('3');
  const created = page.waitForResponse((r) => r.request().method() === 'POST' && /\/v1\/plans$/.test(r.url()));
  await dialog.getByRole('button', { name: '创建' }).click();
  const body = (await (await created).json()) as { bytes_per_cycle: number; status: string };
  expect(body.bytes_per_cycle).toBe(107374182400);
  expect(body.status).toBe('draft');
  await expect(page.getByRole('heading', { name: PLAN, level: 1 })).toBeVisible();
  state.planUrl = page.url();
});

test('关联线路组，新增价格，上架前确认影响（BIL-04、BIL-01、UI-03）', async () => {
  const page = state.page!;
  const groups = page.getByRole('region', { name: '线路组' });
  await groups.getByLabel('添加线路组').selectOption({ label: GROUP });
  await groups.getByRole('button', { name: '添加' }).click();
  await expect(groups.getByRole('button', { name: `从套餐移除线路组 ${GROUP}` })).toBeVisible();

  const prices = page.getByRole('region', { name: '价格' });
  await prices.getByRole('button', { name: '新增价格' }).click();
  const dialog = page.getByRole('dialog', { name: '新增价格' });
  await dialog.getByLabel('周期').selectOption('month');
  await dialog.getByLabel(/^金额（/).fill('30');
  await dialog.getByRole('button', { name: '创建' }).click();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await expect(prices.getByRole('cell', { name: '¥30.00', exact: true })).toBeVisible();
  // 价格行没有编辑入口（BIL-01）。
  await expect(prices.getByRole('button', { name: /编辑/ })).toHaveCount(0);

  const basic = page.getByRole('region', { name: '基本信息' });
  await basic.getByLabel('状态', { exact: true }).selectOption('on_sale');
  await basic.getByRole('button', { name: '保存' }).click();
  const confirm = page.getByRole('dialog', { name: '确认修改套餐？' });
  await expect(confirm).toContainText('将影响 0 名用户。');
  await confirm.getByRole('button', { name: '确认修改' }).click();
  await expect(page.getByRole('status')).toHaveText('已保存。');
});

test('停售在售套餐的最后一个在售价格被拒绝（BIL-01）', async () => {
  const page = state.page!;
  const prices = page.getByRole('region', { name: '价格' });
  await prices.getByRole('button', { name: /^停售 月付/ }).click();
  const confirm = page.getByRole('dialog', { name: '停售价格？' });
  await confirm.getByRole('button', { name: '停售' }).click();
  await expect(confirm.getByRole('alert')).toContainText('最后一个在售价格');
  await confirm.getByRole('button', { name: '取消' }).click();
});

test('移除线路组：影响确认、原因与重新验证（BIL-04、AUTH-19）；之后重新关联', async () => {
  const page = state.page!;
  const groups = page.getByRole('region', { name: '线路组' });
  await groups.getByRole('button', { name: `从套餐移除线路组 ${GROUP}` }).click();
  const confirm = page.getByRole('dialog', { name: `移除线路组 ${GROUP}？` });
  await expect(confirm).toContainText('将影响 0 名用户。');
  await confirm.getByLabel('操作原因').fill('端到端测试');
  await submitSensitive(
    page,
    state.admin!,
    () => confirm.getByRole('button', { name: '移除' }).click(),
    () => expect(groups.getByText('尚未关联线路组。')).toBeVisible({ timeout: 60_000 }),
  );
  await groups.getByLabel('添加线路组').selectOption({ label: GROUP });
  await groups.getByRole('button', { name: '添加' }).click();
  await expect(groups.getByRole('button', { name: `从套餐移除线路组 ${GROUP}` })).toBeVisible();
  expect(state.errors).toEqual([]);
});

test('用户中心套餐页列出该套餐（BIL-21）', async ({ browser }) => {
  const page = await (await browser.newContext()).newPage();
  const errors = collectPageErrors(page);
  const email = `e2e-plans-${randomBytes(6).toString('hex')}@example.com`;
  const password = `pw-${randomBytes(8).toString('hex')}`;
  const seen = await seenMessages(email);
  await page.goto(`${PORTAL}register`);
  await page.getByLabel('邮箱').fill(email);
  await page.getByLabel('密码').fill(password);
  await page.getByRole('button', { name: '注册' }).click();
  const code = verificationCode(await nextMessageText(email, seen));
  await page.getByLabel('验证码').fill(code);
  await page.getByRole('button', { name: '验证', exact: true }).click();
  await expect(page.getByText('邮箱已验证。')).toBeVisible();

  await page.goto(`${PORTAL}login`);
  await page.getByLabel('邮箱').fill(email);
  await page.getByLabel('密码').fill(password);
  await page.getByRole('button', { name: '登录' }).click();
  await expect(page.getByRole('heading', { name: '概览' })).toBeVisible();
  await page.getByRole('link', { name: '套餐', exact: true }).first().click();
  const card = page.getByRole('article', { name: PLAN });
  await expect(card).toContainText('30.00');
  await expect(card).toContainText('100 GiB');
  await expect(card.getByRole('button', { name: '购买' })).toBeDisabled();
  expect(errors).toEqual([]);
});
