// SPDX-License-Identifier: AGPL-3.0-or-later
// M1-04：用户中心套餐页（Mock）。数据为 OpenAPI 示例（标准版，月付与年付）。
import type { Page } from '@playwright/test';
import { PORTAL, expect, test } from './fixtures';

async function signIn(page: Page) {
  await page.goto(`${PORTAL}/login`);
  await page.getByLabel('邮箱').fill('alice@example.com');
  await page.getByLabel('密码').fill('correct-horse-battery');
  await page.getByRole('button', { name: '登录' }).click();
  await expect(page.getByRole('heading', { name: '概览' })).toBeVisible();
}

test('套餐对比与周期选择；购买暂不可用', async ({ page }) => {
  await signIn(page);
  await page.getByRole('link', { name: '套餐', exact: true }).first().click();
  const card = page.getByRole('article', { name: '标准版' });
  await expect(card).toContainText('¥30.00/ 月');
  await expect(card).toContainText('200 GiB');
  await expect(card).toContainText('可访问地区数12');
  await expect(card.getByRole('button', { name: '购买' })).toBeDisabled();
  // 键盘：方向键在周期之间切换。
  await page.getByRole('radio', { name: '月付' }).focus();
  await page.keyboard.press('ArrowRight');
  await expect(page.getByRole('radio', { name: '年付' })).toBeChecked();
  await expect(card).toContainText('¥300.00/ 年');
});

test('套餐页在窄屏下不产生页面横向滚动 @mobile', async ({ page }) => {
  await signIn(page);
  await page.goto(`${PORTAL}/plans`);
  await expect(page.getByRole('article', { name: '标准版' })).toBeVisible();
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow).toBeLessThanOrEqual(0);
});
