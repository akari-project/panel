// SPDX-License-Identifier: AGPL-3.0-or-later
// M1-02：管理员首次登录绑定 TOTP、按权限显示菜单、邀请与撤销（敏感操作）、接受邀请。Mock 网关的约定见 mock/gateway.mjs。
import type { Page } from '@playwright/test';
import { ADMIN, expect, test } from './fixtures';

async function signIn(page: Page, email: string) {
  await page.goto(`${ADMIN}/login`);
  await page.getByLabel('邮箱').fill(email);
  await page.getByLabel('密码').fill('correct-horse-battery');
  await page.getByRole('button', { name: '登录' }).click();
  await page.getByLabel('验证码').fill('492871');
  await page.getByRole('button', { name: '验证', exact: true }).click();
}

test('首次登录绑定 TOTP：显示二维码，展示恢复码后进入后台（AUTH-21）', async ({ page }) => {
  await page.goto(`${ADMIN}/login`);
  await page.getByLabel('邮箱').fill('super+new@example.com');
  await page.getByLabel('密码').fill('correct-horse-battery');
  await page.getByRole('button', { name: '登录' }).click();
  await expect(page.getByRole('img', { name: '绑定身份验证器的二维码' })).toBeVisible();
  await expect(page.getByRole('button', { name: '改用恢复码' })).toHaveCount(0);
  await page.getByLabel('验证码').fill('492871');
  await page.getByRole('button', { name: '验证', exact: true }).click();

  await expect(page.getByRole('heading', { name: '请保存恢复码' })).toBeVisible();
  await expect(page.getByRole('list', { name: '恢复码' }).getByRole('listitem')).toHaveCount(10);
  await page.getByRole('button', { name: '我已保存' }).click();
  await expect(page).toHaveURL(`${ADMIN}/`);
  await expect(page.getByRole('navigation', { name: '主导航' }).getByRole('link', { name: '审计日志' })).toBeVisible();
});

test('邀请与撤销：原因、重新验证与二次确认，5 分钟内复用断言（AUTH-19）', async ({ page }) => {
  await signIn(page, 'super@example.com');
  await page.getByRole('navigation', { name: '主导航' }).getByRole('link', { name: '管理员' }).click();
  await page.getByRole('button', { name: '邀请管理员' }).click();
  const invite = page.getByRole('dialog', { name: '邀请管理员' });
  await invite.getByLabel('邮箱').fill('new.ops@example.com');
  await invite.getByRole('checkbox', { name: '运营' }).check();
  await invite.getByLabel('操作原因').fill('新同事入职');
  await invite.getByRole('button', { name: '发送邀请' }).click();

  const stepUp = page.getByRole('dialog', { name: '验证身份' });
  await stepUp.getByLabel('验证码').fill('000000');
  await stepUp.getByRole('button', { name: '验证' }).click();
  await expect(stepUp.getByLabel('验证码')).toHaveAccessibleDescription('不正确');
  await stepUp.getByLabel('验证码').fill('492871');
  await stepUp.getByRole('button', { name: '验证' }).click();
  await expect(page.getByRole('status')).toHaveText('已向 new.ops@example.com 发送邀请。');

  await page.getByRole('link', { name: '邀请', exact: true }).click();
  await page.getByRole('button', { name: /^撤销对 .+ 的邀请$/ }).first().click();
  const confirm = page.getByRole('dialog', { name: '撤销邀请？' });
  await confirm.getByLabel('操作原因').fill('邮箱填错');
  const revoked = page.waitForResponse((r) => r.request().method() === 'DELETE' && r.url().includes('/v1/staff-invitations/'));
  await confirm.getByRole('button', { name: '撤销' }).click();
  expect((await revoked).status()).toBe(204);
  await expect(page.getByRole('dialog')).toHaveCount(0);
});

test('客服只看到概览，直接访问审计日志显示无权限（AUTH-17）', async ({ page }) => {
  await signIn(page, 'support@example.com');
  const nav = page.getByRole('navigation', { name: '主导航' });
  await expect(nav.getByRole('link', { name: '概览' })).toBeVisible();
  await expect(nav.getByRole('link')).toHaveCount(1);
  await page.goto(`${ADMIN}/audit-logs`);
  await expect(page.getByRole('alert')).toHaveText('没有权限执行此操作。');
});

test('接受邀请：令牌从地址栏移除，新账号设置密码后加入（AUTH-22）', async ({ page }) => {
  await page.goto(`${ADMIN}/accept-invitation#token=inv_new`);
  await expect(page).toHaveURL(`${ADMIN}/accept-invitation`);
  await page.getByRole('button', { name: '接受邀请' }).click();
  await expect(page.getByLabel('设置密码')).toHaveAccessibleDescription(/该邮箱还没有账号，请设置密码。/);
  await page.getByLabel('设置密码').fill('correct-horse-battery');
  await page.getByRole('button', { name: '接受邀请' }).click();
  await expect(page.getByRole('status')).toContainText('已加入管理后台');
  await page.getByRole('link', { name: '前往登录' }).click();
  await expect(page.getByRole('heading', { name: '管理后台登录' })).toBeVisible();
});

test('审计日志在窄屏下不产生页面横向滚动 @mobile', async ({ page }) => {
  await signIn(page, 'super@example.com');
  await page.goto(`${ADMIN}/audit-logs`);
  await expect(page.getByRole('table', { name: '审计日志' })).toBeVisible();
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow).toBeLessThanOrEqual(0);
});
