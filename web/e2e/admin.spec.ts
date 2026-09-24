// SPDX-License-Identifier: AGPL-3.0-or-later
// M0-06 验收 2、4：管理后台挂载在 /admin/ 前缀下，Mock 中完成两步登录并进入空白首页。
import { ADMIN, expect, test } from './fixtures';

test('两步登录后进入首页', async ({ page }) => {
  await page.goto(`${ADMIN}/`);
  await expect(page).toHaveURL(/\/admin\/login\?redirect=%2F$/);
  await expect(page).toHaveTitle('Akari Console');
  await expect(page.getByRole('heading', { name: '管理后台登录' })).toBeVisible();

  await page.getByLabel('邮箱').fill('ops@example.com');
  await page.getByLabel('密码').fill('correct-horse-battery');
  await page.getByRole('button', { name: '登录' }).click();

  // 管理员登录必须完成二次验证（AUTH-21）。
  await expect(page.getByRole('heading', { name: '二次验证' })).toBeVisible();
  await page.getByLabel('验证码').fill('492871');
  await page.getByRole('button', { name: '验证', exact: true }).click();

  await expect(page).toHaveURL(`${ADMIN}/`);
  await expect(page.getByRole('heading', { name: '概览' })).toBeVisible();
  await expect(page.getByText('当前管理员：ops@example.com')).toBeVisible();
  await expect(page.getByRole('link', { name: '源代码' })).toBeVisible();
});

test('可以改用恢复码', async ({ page }) => {
  await page.goto(`${ADMIN}/login`);
  await page.getByLabel('邮箱').fill('ops@example.com');
  await page.getByLabel('密码').fill('correct-horse-battery');
  await page.getByRole('button', { name: '登录' }).click();
  await page.getByRole('button', { name: '改用恢复码' }).click();
  await page.getByLabel('恢复码').fill('abcd-efgh-ijkl');
  await page.getByRole('button', { name: '验证', exact: true }).click();
  await expect(page.getByRole('heading', { name: '概览' })).toBeVisible();
});

test('验证码格式错误时提示', async ({ page }) => {
  await page.goto(`${ADMIN}/login`);
  await page.getByLabel('邮箱').fill('ops@example.com');
  await page.getByLabel('密码').fill('correct-horse-battery');
  await page.getByRole('button', { name: '登录' }).click();
  await page.getByLabel('验证码').fill('12');
  await page.getByRole('button', { name: '验证', exact: true }).click();
  await expect(page.getByLabel('验证码')).toHaveAccessibleDescription('请输入 6 位数字');
});

test('路径前缀下的深层路径', async ({ page }) => {
  await page.goto(`${ADMIN}/users/123`);
  await expect(page.getByRole('heading', { name: '页面不存在' })).toBeVisible();
  await page.getByRole('link', { name: '返回首页' }).click();
  await expect(page).toHaveURL(/\/admin\/login\?redirect=%2F$/);
});

test('服务端不改写相对路径时，挂载路径由入口脚本地址推出', async ({ page }) => {
  await page.goto('http://127.0.0.1:4102/admin/');
  await expect(page).toHaveURL('http://127.0.0.1:4102/admin/login?redirect=%2F');
  await expect(page.getByRole('heading', { name: '管理后台登录' })).toBeVisible();
});
