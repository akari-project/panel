// SPDX-License-Identifier: AGPL-3.0-or-later
import { describe, expect, it } from 'vitest';
import { canOperate, hasPermission, isSensitive } from './permissions';

const operator = { permissions: ['accounts.read', 'plans.*', 'hosts.*'], is_superadmin: false };
const superadmin = { permissions: ['*'], is_superadmin: true };

describe('hasPermission（AUTH-17）', () => {
  it.each([
    [operator, 'none', true],
    [operator, 'accounts.read', true],
    [operator, 'accounts.adjust', false],
    [operator, 'plans.*', true],
    [operator, 'plans.write', true],
    [operator, 'audit.read', false],
    [operator, 'superadmin', false],
    [superadmin, 'audit.read', true],
    [superadmin, 'superadmin', true],
    // `*` 不能代替 superadmin 标志。
    [{ permissions: ['*'], is_superadmin: false }, 'superadmin', false],
    // `资源.*` 只覆盖同名资源，不按前缀匹配。
    [{ permissions: ['location-groups.*'], is_superadmin: false }, 'location.read', false],
  ])('%j 需要 %s → %s', (grant, required, want) => {
    expect(hasPermission(grant, required)).toBe(want);
  });

  it('未取得权限时一律拒绝', () => {
    expect(hasPermission(undefined, 'none')).toBe(false);
  });
});

describe('按操作判断', () => {
  it('取 x-permission 与 x-sensitive', () => {
    expect(canOperate(operator, 'GET /v1/audit-logs')).toBe(false);
    expect(canOperate(superadmin, 'GET /v1/audit-logs')).toBe(true);
    expect(isSensitive('POST /v1/staff-invitations')).toBe(true);
    expect(isSensitive('GET /v1/staff')).toBe(false);
  });
});
