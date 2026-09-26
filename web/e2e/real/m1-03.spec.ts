// SPDX-License-Identifier: AGPL-3.0-or-later
// M1-03 验收 4（server/e2e/portal TestM1_03_PortalPlaywright 驱动，make e2e-portal）：在真实控制面上注册 → 两个浏览器上下文分别登录 →
// 设备列表中看到两条浏览器登录 → 移除另一个 → 被移除的会话回到登录页 → 移除当前设备即登出。
// 两个上下文都是浏览器登录（web 设备），不占用设备名额；名额与凭据的验收由后端测试覆盖。
import { randomBytes } from 'node:crypto';
import { expect, test, type Page } from '@playwright/test';
import { newPage } from './admin';
import { nextMessageText, seenMessages, verificationCode } from './mail';

const email = `e2e-devices-${randomBytes(6).toString('hex')}@example.com`;
const password = `pw-${randomBytes(8).toString('hex')}`;

async function register(page: Page) {
  const seen = await seenMessages(email);
  await page.goto('register');
  await page.getByLabel('邮箱').fill(email);
  await page.getByLabel('密码').fill(password);
  await page.getByRole('button', { name: '注册' }).click();
  await expect(page.getByText(`验证码已发送到 ${email}`)).toBeVisible();
  await page.getByLabel('验证码').fill(verificationCode(await nextMessageText(email, seen)));
  await page.getByRole('button', { name: '验证', exact: true }).click();
  await expect(page.getByText('邮箱已验证。')).toBeVisible();
}

async function signIn(page: Page) {
  await page.goto('login');
  await page.getByLabel('邮箱').fill(email);
  await page.getByLabel('密码').fill(password);
  await page.getByRole('button', { name: '登录' }).click();
  await expect(page.getByRole('heading', { name: '概览' })).toBeVisible();
}

test('设备列表与移除（AUTH-15）', async ({ browser }) => {
  const { page: a, errors: errorsA } = await newPage(browser);
  const { page: b, errors: errorsB } = await newPage(browser);
  await register(a);
  await signIn(a);
  await signIn(b);

  await a.getByRole('link', { name: '设备', exact: true }).first().click();
  await expect(a.getByRole('heading', { name: '设备与登录会话' })).toBeVisible();
  const cards = a.getByRole('list', { name: '设备列表' }).getByRole('article');
  await expect(cards).toHaveCount(2);
  await expect(a.getByText(/^设备名额：已用 0 \/ \d+$/)).toBeVisible();
  const current = cards.filter({ hasText: '当前设备' });
  await expect(current).toHaveCount(1);
  await expect(current).toContainText('平台浏览器');

  // 移除另一个浏览器登录。
  const other = cards.filter({ hasNotText: '当前设备' });
  await other.getByRole('button', { name: /^移除 / }).click();
  await a.getByRole('dialog').getByRole('button', { name: '移除', exact: true }).click();
  await expect(a.getByText(/^已移除 /)).toBeVisible();
  await expect(cards).toHaveCount(1);

  // 被移除的会话无法恢复，回到登录页。
  await b.goto('devices');
  await expect(b.getByRole('heading', { name: '登录' })).toBeVisible();

  // 移除当前设备等同登出。
  await current.getByRole('button', { name: /^移除 / }).click();
  await expect(a.getByRole('dialog')).toContainText('这是你正在使用的设备');
  await a.getByRole('dialog').getByRole('button', { name: '移除', exact: true }).click();
  await expect(a.getByRole('heading', { name: '登录' })).toBeVisible();
  await a.goto('devices');
  await expect(a).toHaveURL(/\/login\?redirect=/);
  expect([...errorsA, ...errorsB]).toEqual([]);
});
