// SPDX-License-Identifier: AGPL-3.0-or-later
// M1-04：套餐、价格行、线路组（Mock）。Prism 不保存状态，响应为 OpenAPI 示例（标准版、亚太标准）；
// 这里验证请求所带的头与界面流程：影响确认（UI-03）、If-Match（CONV-28）、移除线路组的重新验证（AUTH-19）。
// 请求体由组件测试（admin/src/pages/plans.test.tsx）断言：浏览器以流发送的请求体在 Playwright 中取不到。
import type { Page } from '@playwright/test';
import { ADMIN, expect, test } from './fixtures';

async function signIn(page: Page) {
  await page.goto(`${ADMIN}/login`);
  await page.getByLabel('邮箱').fill('super@example.com');
  await page.getByLabel('密码').fill('correct-horse-battery');
  await page.getByRole('button', { name: '登录' }).click();
  await page.getByLabel('验证码').fill('492871');
  await page.getByRole('button', { name: '验证', exact: true }).click();
  await expect(page.getByRole('heading', { name: '概览', level: 1 })).toBeVisible();
}

test('套餐：修改等级前确认影响人数，保存带 If-Match（BIL-26、UI-03）', async ({ page }) => {
  await signIn(page);
  await page.getByRole('navigation', { name: '主导航' }).getByRole('link', { name: '套餐与价格' }).click();
  const table = page.getByRole('table', { name: '套餐与价格' });
  await expect(table.getByText('200 GiB').first()).toBeVisible();
  await table.getByRole('link', { name: '标准版' }).first().click();

  const basic = page.getByRole('region', { name: '基本信息' });
  await basic.getByLabel('等级').fill('3');
  await basic.getByRole('button', { name: '保存' }).click();
  const confirm = page.getByRole('dialog', { name: '确认修改套餐？' });
  await expect(confirm).toContainText('将影响 312 名用户。');
  const patched = page.waitForRequest((r) => r.method() === 'PATCH' && /\/v1\/plans\/[^/]+$/.test(r.url()));
  await confirm.getByRole('button', { name: '确认修改' }).click();
  const req = await patched;
  expect(req.headers()['if-match']).toBe('"p-4f1c9a2e"');
  await expect(page.getByRole('status')).toHaveText('已保存。');
});

test('套餐的线路组：移除前显示影响，原因与重新验证（BIL-04、AUTH-19）', async ({ page }) => {
  await signIn(page);
  await page.goto(`${ADMIN}/plans/01927c3e-8a41-7005-9d3e-5f6a7b8c0005`);
  const section = page.getByRole('region', { name: '线路组' });
  await section.getByRole('button', { name: '从套餐移除线路组 亚太标准' }).click();
  const confirm = page.getByRole('dialog', { name: '移除线路组 亚太标准？' });
  await expect(confirm).toContainText('将影响 312 名用户。');
  await confirm.getByLabel('操作原因').fill('线路调整');
  await confirm.getByRole('button', { name: '移除' }).click();
  const stepUp = page.getByRole('dialog', { name: '验证身份' });
  await stepUp.getByLabel('验证码').fill('492871');
  const removed = page.waitForRequest((r) => r.method() === 'DELETE' && r.url().includes('/location-groups/'));
  await stepUp.getByRole('button', { name: '验证' }).click();
  const req = await removed;
  expect(req.headers()['audit-reason']).toBe(encodeURIComponent('线路调整'));
  expect(req.headers()['mfa-assertion']).toBeTruthy();
  expect(req.headers()['if-match']).toBe('"p-4f1c9a2e"');
  await expect(page.getByRole('dialog')).toHaveCount(0);
});

test('价格行：只能新建与停售，币种为站点结算货币（BIL-01、CONV-08）', async ({ page }) => {
  await signIn(page);
  await page.goto(`${ADMIN}/plans/01927c3e-8a41-7005-9d3e-5f6a7b8c0005`);
  const section = page.getByRole('region', { name: '价格' });
  await section.getByRole('button', { name: '新增价格' }).click();
  const dialog = page.getByRole('dialog', { name: '新增价格' });
  await dialog.getByLabel('周期').selectOption('year');
  await dialog.getByLabel('金额（CNY）').fill('299.9');
  const created = page.waitForRequest((r) => r.method() === 'POST' && r.url().endsWith('/prices'));
  await dialog.getByRole('button', { name: '创建' }).click();
  expect((await created).headers()['idempotency-key']).toMatch(/^[0-9a-f-]{36}$/);
  await expect(page.getByRole('dialog')).toHaveCount(0);

  await section.getByRole('button', { name: '停售 月付 ¥30.00' }).first().click();
  const confirm = page.getByRole('dialog', { name: '停售价格？' });
  const discontinued = page.waitForRequest((r) => r.method() === 'PATCH' && r.url().includes('/prices/'));
  await confirm.getByRole('button', { name: '停售' }).click();
  const req = await discontinued;
  expect(req.headers()['if-match']).toBeTruthy();
  await expect(page.getByRole('dialog')).toHaveCount(0);
});

test('线路组：修改最低等级前确认影响人数（ACS-05）', async ({ page }) => {
  await signIn(page);
  await page.getByRole('navigation', { name: '主导航' }).getByRole('link', { name: '线路组' }).click();
  await page.getByRole('button', { name: '编辑线路组 亚太标准' }).click();
  const dialog = page.getByRole('dialog', { name: '编辑线路组 亚太标准' });
  await dialog.getByLabel('最低等级').fill('3');
  await dialog.getByRole('button', { name: '保存' }).click();
  const confirm = page.getByRole('dialog', { name: '确认修改最低等级？' });
  await expect(confirm).toContainText('将影响 1208 名用户。');
  await confirm.getByRole('button', { name: '确认修改' }).click();
  await expect(page.getByRole('dialog')).toHaveCount(0);
});

test('套餐列表在窄屏下不产生页面横向滚动 @mobile', async ({ page }) => {
  await signIn(page);
  await page.goto(`${ADMIN}/plans`);
  await expect(page.getByRole('table', { name: '套餐与价格' })).toBeVisible();
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow).toBeLessThanOrEqual(0);
});
