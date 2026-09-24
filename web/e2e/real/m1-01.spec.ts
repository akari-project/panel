// SPDX-License-Identifier: AGPL-3.0-or-later
// M1-01 验收 4：在真实控制面上完成注册 → 邮箱验证 → 登录 → 启用 TOTP → 二次验证登录，以及找回密码。
// 验证码与重置链接从 Mailpit 读取；TOTP 由测试按 RFC 6238 计算。每个测试使用随机邮箱。
import { randomBytes } from 'node:crypto';
import { expect, test, type Page } from '@playwright/test';
import { nextMessageText, resetLink, seenMessages, verificationCode } from './mail';
import { freshTotp } from './totp';

const randomEmail = (tag: string) => `e2e-${tag}-${randomBytes(6).toString('hex')}@example.com`;

// 接口的 4xx（未登录时的 /v1/me、刷新令牌失效等）属于预期；其余控制台错误与未捕获异常使测试失败。
function collectPageErrors(page: Page) {
  const errors: string[] = [];
  page.on('console', (m) => {
    const expected = /status of 4\d\d/.test(m.text()) && /\/v1\//.test(m.location().url);
    if (m.type() === 'error' && !expected) errors.push(m.text());
  });
  page.on('pageerror', (e) => errors.push(String(e)));
  return errors;
}

async function register(page: Page, email: string, password: string) {
  const seen = await seenMessages(email);
  await page.goto('login');
  await page.getByRole('link', { name: '注册新账号' }).click();
  await page.getByLabel('邮箱').fill(email);
  await page.getByLabel('密码').fill(password);
  await page.getByRole('button', { name: '注册' }).click();
  await expect(page.getByRole('heading', { name: '验证邮箱' })).toBeVisible();
  await expect(page.getByText(`验证码已发送到 ${email}`)).toBeVisible();

  const code = verificationCode(await nextMessageText(email, seen));
  await page.getByLabel('验证码').fill(code);
  await page.getByRole('button', { name: '验证', exact: true }).click();
  await expect(page.getByText('邮箱已验证。')).toBeVisible();
}

async function signInPassword(page: Page, email: string, password: string) {
  await page.goto('login');
  await page.getByLabel('邮箱').fill(email);
  await page.getByLabel('密码').fill(password);
  await page.getByRole('button', { name: '登录' }).click();
}

async function signOut(page: Page) {
  await page.getByRole('button', { name: '退出登录' }).click();
  await expect(page.getByRole('heading', { name: '登录' })).toBeVisible();
}

test('注册、验证邮箱、登录、启用 TOTP、二次验证登录', async ({ page }) => {
  const errors = collectPageErrors(page);
  const email = randomEmail('mfa');
  const password = `pw-${randomBytes(8).toString('hex')}`;
  const newPassword = `pw-${randomBytes(8).toString('hex')}`;

  await register(page, email, password);

  // 登录：邮箱已验证，概览不再提示验证。
  await signInPassword(page, email, password);
  await expect(page.getByRole('heading', { name: '概览' })).toBeVisible();
  await expect(page.getByText(`欢迎，${email}`)).toBeVisible();
  await expect(page.getByText('你的邮箱尚未验证')).toHaveCount(0);

  // 启用 TOTP：读取页面显示的 Base32 密钥，计算验证码。
  await page.getByRole('link', { name: '账号安全' }).click();
  await expect(page.getByText('未启用')).toBeVisible();
  await page.getByRole('button', { name: '启用二次验证' }).click();
  await expect(page.getByRole('img', { name: '身份验证器二维码' })).toBeVisible();
  const secret = (await page.getByLabel('密钥').textContent())!.replace(/\s/g, '');
  expect(secret).toMatch(/^[A-Z2-7]+=*$/);
  const activation = await freshTotp(secret);
  await page.getByLabel('验证码').fill(activation.code);
  await page.getByRole('button', { name: '确认启用' }).click();

  // 一次性展示 10 个恢复码，保存后回到状态视图。
  const items = page.getByRole('list', { name: '恢复码' }).getByRole('listitem');
  await expect(items).toHaveCount(10);
  const recoveryCodes = (await items.allTextContents()).map((c) => c.trim());
  const download = page.waitForEvent('download');
  await page.getByRole('button', { name: '下载' }).click();
  expect((await download).suggestedFilename()).toBe('recovery-codes.txt');
  await page.getByRole('button', { name: '我已保存' }).click();
  await expect(page.getByText('已启用')).toBeVisible();

  // 修改密码需要重新验证（AUTH-23）；以密码完成的登录视为一次重新验证，登录后 5 分钟内直接生效，不弹出验证框。
  // 窗口过期后的验证框由 Go 测试（TestStepUp、TestLoginCountsAsReauth）与 Mock 端到端测试覆盖。
  await page.getByLabel('新密码', { exact: true }).fill(newPassword);
  await page.getByLabel('确认新密码').fill(newPassword);
  await page.getByRole('button', { name: '修改密码' }).click();
  await expect(page.getByText('密码已修改。')).toBeVisible();
  await expect(page.getByRole('dialog', { name: '验证身份' })).toHaveCount(0);

  // 退出后重新登录：第一步返回 mfa_required，第二步提交 TOTP。
  await signOut(page);
  await signInPassword(page, email, newPassword);
  await expect(page.getByRole('heading', { name: '二次验证' })).toBeVisible();
  const login = await freshTotp(secret, activation.step);
  await page.getByLabel('验证码').fill(login.code);
  await page.getByRole('button', { name: '验证', exact: true }).click();
  await expect(page.getByRole('heading', { name: '概览' })).toBeVisible();

  // 再次退出，改用恢复码完成第二步。
  await signOut(page);
  await signInPassword(page, email, newPassword);
  await page.getByRole('button', { name: '改用恢复码' }).click();
  await page.getByLabel('恢复码').fill(recoveryCodes[0]!);
  await page.getByRole('button', { name: '验证', exact: true }).click();
  await expect(page.getByRole('heading', { name: '概览' })).toBeVisible();

  expect(errors).toEqual([]);
});

test('找回密码：邮件链接设置新密码后可以登录', async ({ page }) => {
  const errors = collectPageErrors(page);
  const email = randomEmail('reset');
  const password = `pw-${randomBytes(8).toString('hex')}`;
  const newPassword = `pw-${randomBytes(8).toString('hex')}`;
  await register(page, email, password);

  const seen = await seenMessages(email);
  await page.goto('login');
  await page.getByRole('link', { name: '忘记密码？' }).click();
  await page.getByLabel('邮箱').fill(email);
  await page.getByRole('button', { name: '发送重置链接' }).click();
  await expect(page.getByRole('status')).toContainText('重置链接已发送');

  const link = resetLink(await nextMessageText(email, seen));
  expect(link.pathname).toMatch(/\/reset-password$/);
  await page.goto(link.href);
  await expect(page.getByLabel('新密码', { exact: true })).toBeVisible();
  // 令牌读取后立即从地址栏清除。
  expect(new URL(page.url()).hash).toBe('');
  await page.getByLabel('新密码', { exact: true }).fill(newPassword);
  await page.getByLabel('确认新密码').fill(newPassword);
  await page.getByRole('button', { name: '重置密码' }).click();
  await expect(page.getByText(/密码已重置/)).toBeVisible();

  // 旧密码失效，新密码可以登录。
  await signInPassword(page, email, password);
  await expect(page.getByRole('alert')).toContainText('邮箱、密码或验证码不正确。');
  await signInPassword(page, email, newPassword);
  await expect(page.getByRole('heading', { name: '概览' })).toBeVisible();

  // 同一链接不能再次使用。
  await page.goto(link.href);
  await page.getByLabel('新密码', { exact: true }).fill(password);
  await page.getByLabel('确认新密码').fill(password);
  await page.getByRole('button', { name: '重置密码' }).click();
  await expect(page.getByRole('alert')).toContainText('重置链接无效或已被使用。');

  expect(errors).toEqual([]);
});

test('访问令牌过期时用刷新令牌 Cookie 续期；退出后无法续期', async ({ page, context }) => {
  const errors = collectPageErrors(page);
  const email = randomEmail('refresh');
  const password = `pw-${randomBytes(8).toString('hex')}`;
  await register(page, email, password);
  await signInPassword(page, email, password);
  await expect(page.getByRole('heading', { name: '概览' })).toBeVisible();

  // 模拟访问令牌过期：只删除访问令牌 Cookie，刷新令牌仍在（AUTH-08）。
  const cookies = await context.cookies();
  expect(cookies.map((c) => c.name)).toEqual(expect.arrayContaining(['__Host-access_token', '__Host-refresh_token']));
  await context.clearCookies({ name: '__Host-access_token' });
  const refreshed = page.waitForResponse((r) => r.url().endsWith('/v1/oauth/token'));
  await page.goto('security');
  expect((await refreshed).status()).toBe(200);
  await expect(page.getByRole('heading', { name: '账号安全' })).toBeVisible();
  expect((await context.cookies()).map((c) => c.name)).toContain('__Host-access_token');

  // 退出后刷新令牌已吊销：需要登录的页面回到登录页。
  await signOut(page);
  await page.goto('security');
  await expect(page).toHaveURL(/\/login\?redirect=%2Fsecurity$/);

  expect(errors).toEqual([]);
});
