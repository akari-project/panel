// SPDX-License-Identifier: AGPL-3.0-or-later
// M1-01：用户中心的注册、邮箱验证、找回密码、账号安全与重新验证（Mock）。
// Mock 网关的约定见 mock/gateway.mjs 顶部注释。
import type { Page } from '@playwright/test';
import { PORTAL, expect, test } from './fixtures';

const TOKEN = 'q3v8fJb1l0m9Kp3yVZ4aQeX2nR6tUoHcFgWjD5sLkP0';

async function signIn(page: Page, email = 'alice@example.com', to = '/') {
  await page.goto(`${PORTAL}/login?redirect=${encodeURIComponent(to)}`);
  await page.getByLabel('邮箱').fill(email);
  await page.getByLabel('密码').fill('correct-horse-battery');
  await page.getByRole('button', { name: '登录' }).click();
}

test('注册后验证邮箱', async ({ page }) => {
  await page.goto(`${PORTAL}/login`);
  await page.getByRole('link', { name: '注册新账号' }).click();
  await expect(page.getByRole('heading', { name: '注册' })).toBeVisible();
  await page.getByLabel('邮箱').fill('bob@example.com');
  await page.getByLabel('密码').fill('correct-horse-battery');
  await page.getByRole('button', { name: '注册' }).click();

  await expect(page).toHaveURL(`${PORTAL}/verify-email?email=bob%40example.com`);
  await expect(page.getByText('验证码已发送到 bob@example.com')).toBeVisible();
  await expect(page.getByRole('button', { name: /重新发送（\d+ 秒）/ })).toBeDisabled();

  // 验证码无效（Mock：000000）时提示在字段上。
  await page.getByLabel('验证码').fill('000000');
  await page.getByRole('button', { name: '验证', exact: true }).click();
  await expect(page.getByLabel('验证码')).toHaveAccessibleDescription('无效');

  await page.getByLabel('验证码').fill('482913');
  await page.getByRole('button', { name: '验证', exact: true }).click();
  await expect(page.getByText('邮箱已验证。')).toBeVisible();
  await page.getByRole('link', { name: '去登录' }).click();
  await expect(page.getByRole('heading', { name: '登录' })).toBeVisible();
});

test('注册关闭时的提示', async ({ page }) => {
  await page.goto(`${PORTAL}/register`);
  await page.getByLabel('邮箱').fill('closed@example.com');
  await page.getByLabel('密码').fill('correct-horse-battery');
  await page.getByRole('button', { name: '注册' }).click();
  await expect(page.getByRole('alert')).toContainText('当前不开放注册，或需要填写有效的邀请码。');
});

test('重新发送被限流时按 Retry-After 倒计时', async ({ page }) => {
  await page.goto(`${PORTAL}/verify-email`);
  await page.getByLabel('邮箱').fill('limited@example.com');
  await page.getByRole('button', { name: '重新发送验证码' }).click();
  await expect(page.getByRole('button', { name: /重新发送（(30|29) 秒）/ })).toBeDisabled();
  await expect(page.getByRole('alert')).toContainText('操作过于频繁');
});

test('已登录时验证邮箱只需验证码', async ({ page }) => {
  await signIn(page, 'unverified@example.com');
  await expect(page.getByText('你的邮箱尚未验证')).toBeVisible();
  await page.getByRole('link', { name: '去验证' }).click();
  await expect(page.getByLabel('验证码')).toBeFocused();
  await expect(page.getByLabel('邮箱')).toHaveCount(0);
  await page.keyboard.type('482913');
  await page.keyboard.press('Enter');
  await expect(page.getByText('邮箱已验证。')).toBeVisible();
  await page.getByRole('link', { name: '返回概览' }).click();
  await expect(page.getByText('你的邮箱尚未验证')).toHaveCount(0);
});

test('找回密码并用链接重置', async ({ page }) => {
  await page.goto(`${PORTAL}/login`);
  await page.getByRole('link', { name: '忘记密码？' }).click();
  await page.getByLabel('邮箱').fill('alice@example.com');
  await page.getByRole('button', { name: '发送重置链接' }).click();
  await expect(page.getByRole('status')).toContainText('重置链接已发送');

  await page.goto(`${PORTAL}/reset-password#token=${TOKEN}`);
  await expect(page.getByLabel('新密码', { exact: true })).toBeVisible();
  // 令牌读取后立即从地址栏清除。
  await expect(page).toHaveURL(`${PORTAL}/reset-password`);
  await page.getByLabel('新密码', { exact: true }).fill('new-correct-horse');
  await page.getByLabel('确认新密码').fill('new-correct-horse');
  await page.getByRole('button', { name: '重置密码' }).click();
  await expect(page.getByText(/密码已重置/)).toBeVisible();
});

test('启用 TOTP 并保存恢复码', async ({ page }) => {
  await signIn(page, 'alice@example.com', '/security');
  await expect(page.getByRole('heading', { name: '账号安全' })).toBeVisible();
  await expect(page.getByText('未启用')).toBeVisible();
  await page.getByRole('button', { name: '启用二次验证' }).click();

  await expect(page.getByRole('img', { name: '身份验证器二维码' })).toBeVisible();
  await expect(page.getByLabel('密钥')).toHaveAttribute('data-secret', 'JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP');
  await page.getByLabel('验证码').fill('731946');
  await page.getByRole('button', { name: '确认启用' }).click();

  const codes = page.getByRole('list', { name: '恢复码' }).getByRole('listitem');
  await expect(codes).toHaveCount(10);
  await expect(page.getByRole('heading', { name: '请保存恢复码' })).toBeFocused();
  const download = page.waitForEvent('download');
  await page.getByRole('button', { name: '下载' }).click();
  expect((await download).suggestedFilename()).toBe('recovery-codes.txt');

  await page.getByRole('button', { name: '我已保存' }).click();
  await expect(page.getByText('已启用')).toBeVisible();
});

/** 登录时获得的重新验证窗口（AUTH-23）过期。 */
async function expireReauth(page: Page) {
  await page.context().clearCookies({ name: 'panel_mock_client_reauth' });
}

test('修改密码需要重新验证，完成后自动继续', async ({ page }) => {
  await signIn(page, 'carol@example.com', '/security');
  await expireReauth(page);
  await page.getByLabel('新密码', { exact: true }).fill('new-correct-horse');
  await page.getByLabel('确认新密码').fill('new-correct-horse');
  await page.getByRole('button', { name: '修改密码' }).click();

  const dialog = page.getByRole('dialog', { name: '验证身份' });
  await expect(dialog).toBeVisible();
  // 焦点在对话框内的密码框上，可以直接用键盘完成。
  await expect(dialog.getByRole('textbox', { name: '密码' })).toBeFocused();
  await page.keyboard.type('wrong-password');
  await page.keyboard.press('Enter');
  await expect(dialog.getByRole('textbox', { name: '密码' })).toHaveAccessibleDescription('不正确');
  await dialog.getByRole('textbox', { name: '密码' }).fill('correct-horse-battery');
  await page.keyboard.press('Enter');
  await expect(dialog).toBeHidden();
  await expect(page.getByText('密码已修改。')).toBeVisible();
});

test('停用二次验证：二次确认、重新验证后生效', async ({ page }) => {
  await signIn(page, 'mfa@example.com');
  await expect(page.getByRole('heading', { name: '二次验证' })).toBeVisible();
  await page.getByLabel('验证码').fill('731946');
  await page.getByRole('button', { name: '验证', exact: true }).click();
  await page.getByRole('link', { name: '账号安全' }).click();
  await expect(page.getByText('已启用')).toBeVisible();
  await expireReauth(page);

  await page.getByRole('button', { name: '停用二次验证' }).click();
  const confirm = page.getByRole('dialog', { name: '停用二次验证？' });
  await expect(confirm).toContainText('现有的恢复码全部作废');
  await confirm.getByRole('button', { name: '停用二次验证' }).click();

  const dialog = page.getByRole('dialog', { name: '验证身份' });
  await dialog.getByRole('radio', { name: '验证码' }).check();
  await dialog.getByRole('textbox', { name: '验证码' }).fill('105277');
  await dialog.getByRole('button', { name: '验证并继续' }).click();
  await expect(page.getByText('未启用')).toBeVisible();
});

test('访问令牌过期时自动刷新；会话失效时回到登录页', async ({ page, context }) => {
  await signIn(page, 'alice@example.com', '/security');
  await expect(page.getByRole('heading', { name: '账号安全' })).toBeVisible();

  // 模拟访问令牌过期：刷新令牌仍在。
  await context.clearCookies({ name: 'panel_mock_client_access' });
  const refreshed = page.waitForResponse((r) => r.url().endsWith('/v1/oauth/token') && r.status() === 200);
  await page.reload();
  await refreshed;
  await expect(page.getByRole('heading', { name: '账号安全' })).toBeVisible();

  // 会话失效：刷新失败后回到登录页，登录后回到原页面。
  await context.clearCookies();
  await page.getByLabel('新密码', { exact: true }).fill('new-correct-horse');
  await page.getByLabel('确认新密码').fill('new-correct-horse');
  await page.getByRole('button', { name: '修改密码' }).click();
  await expect(page).toHaveURL(`${PORTAL}/login?redirect=%2Fsecurity`);
});

test('登录超过 5 分钟后开始绑定 TOTP 需要重新验证', async ({ page }) => {
  await signIn(page, 'dave@example.com', '/security');
  await expireReauth(page);
  await page.getByRole('button', { name: '启用二次验证' }).click();
  const dialog = page.getByRole('dialog', { name: '验证身份' });
  await expect(dialog).toBeVisible();
  await dialog.getByRole('textbox', { name: '密码' }).fill('correct-horse');
  await dialog.getByRole('button', { name: '验证并继续' }).click();
  await expect(page.getByRole('img', { name: '身份验证器二维码' })).toBeVisible();
});

test('移动端：账号安全与恢复码 @mobile', async ({ page }) => {
  await signIn(page, 'alice@example.com', '/security');
  await page.getByRole('button', { name: '启用二次验证' }).click();
  await expect(page.getByRole('img', { name: '身份验证器二维码' })).toBeInViewport();
  await page.getByLabel('验证码').fill('731946');
  await page.getByRole('button', { name: '确认启用' }).click();
  await expect(page.getByRole('list', { name: '恢复码' }).getByRole('listitem')).toHaveCount(10);
  // 不出现横向滚动。
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
});
