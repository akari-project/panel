// SPDX-License-Identifier: AGPL-3.0-or-later
// M1-03：用户中心的设备与登录会话（Mock）。数据为 OpenAPI 示例（iPhone 持有凭据，当前设备为 Firefox 浏览器，上限 3）。
// 移除设备与名额已满的模拟见 mock/gateway.mjs 顶部注释。
import type { Page } from '@playwright/test';
import { PORTAL, expect, test } from './fixtures';

async function signIn(page: Page, email = 'alice@example.com') {
  await page.goto(`${PORTAL}/login`);
  await page.getByLabel('邮箱').fill(email);
  await page.getByLabel('密码').fill('correct-horse-battery');
  await page.getByRole('button', { name: '登录' }).click();
  await expect(page.getByRole('heading', { name: '概览' })).toBeVisible();
}

test('查看设备列表并移除其他设备', async ({ page }) => {
  await signIn(page);
  await page.getByRole('link', { name: '设备', exact: true }).first().click();
  await expect(page.getByRole('heading', { name: '设备与登录会话' })).toBeVisible();
  await expect(page.getByText('设备名额：已用 1 / 3')).toBeVisible();
  const phone = page.getByRole('article', { name: 'iPhone17,1' });
  await expect(phone).toContainText('203.0.113.0/24');
  await expect(phone).toContainText('已下发');
  await expect(page.getByRole('article', { name: /Firefox 131 on Windows/ })).toContainText('当前设备');

  // 键盘：Esc 关闭确认框，焦点回到移除按钮。
  const remove = page.getByRole('button', { name: '移除 iPhone17,1' });
  await remove.focus();
  await page.keyboard.press('Enter');
  const dialog = page.getByRole('dialog', { name: '移除 iPhone17,1？' });
  await expect(dialog).toContainText('代理凭据立即吊销');
  await page.keyboard.press('Escape');
  await expect(dialog).toHaveCount(0);
  await expect(remove).toBeFocused();

  await remove.click();
  await dialog.getByRole('button', { name: '移除', exact: true }).click();
  await expect(page.getByText('已移除 iPhone17,1。')).toBeVisible();
  await expect(phone).toHaveCount(0);
  await expect(page.getByText('设备名额：已用 0 / 3')).toBeVisible();
});

test('移除当前设备后退出登录', async ({ page }) => {
  await signIn(page);
  await page.goto(`${PORTAL}/devices`);
  await page.getByRole('button', { name: '移除 Firefox 131 on Windows' }).click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toContainText('这是你正在使用的设备');
  await dialog.getByRole('button', { name: '移除', exact: true }).click();
  await expect(page).toHaveURL(`${PORTAL}/login`);
  // 会话已吊销：再访问需要登录的页面会回到登录页。
  await page.goto(`${PORTAL}/devices`);
  await expect(page).toHaveURL(/\/login\?redirect=/);
});

test('名额已满时提示', async ({ page }) => {
  await signIn(page, 'full@example.com');
  await page.goto(`${PORTAL}/devices`);
  await expect(page.getByText('设备名额：已用 1 / 1')).toBeVisible();
  await expect(page.getByRole('status')).toContainText('设备名额已用完');
});

test('设备页在窄屏下不产生页面横向滚动 @mobile', async ({ page }) => {
  await signIn(page);
  await page.goto(`${PORTAL}/devices`);
  await expect(page.getByRole('article', { name: 'iPhone17,1' })).toBeVisible();
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow).toBeLessThanOrEqual(0);
});
