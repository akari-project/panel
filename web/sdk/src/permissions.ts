// SPDX-License-Identifier: AGPL-3.0-or-later
// 管理员权限判断（spec/10 AUTH-17）。权限来自 GET /v1/staff/me 展开后的 permissions；
// 每个操作所需的权限来自 OpenAPI 的 x-permission（生成在 console.ops.gen.ts）。
// 这里只决定界面上是否显示菜单与按钮，服务端中间件仍会校验（403 forbidden）。
import { consoleOperations } from './console.ops.gen';

export { consoleOperations };
export type ConsoleOperation = keyof typeof consoleOperations;

export interface StaffGrant {
  permissions: readonly string[];
  is_superadmin: boolean;
}

/**
 * 判断是否拥有 required：
 * - `none`：只要求已登录；
 * - `superadmin`：只看 is_superadmin，不能由任何权限授予；
 * - `*` 覆盖全部权限；`资源.*` 覆盖该资源的全部动作；其余须完全相同。
 */
export function hasPermission(grant: StaffGrant | undefined, required: string): boolean {
  if (!grant) return false;
  if (required === 'none') return true;
  if (required === 'superadmin') return grant.is_superadmin;
  const resource = required.slice(0, required.lastIndexOf('.'));
  return grant.permissions.some((p) => p === '*' || p === required || (p.endsWith('.*') && p.slice(0, -2) === resource));
}

/** 能否执行某个管理接口操作，键为“方法 路径”，例如 `POST /v1/roles`。 */
export function canOperate(grant: StaffGrant | undefined, op: ConsoleOperation): boolean {
  return hasPermission(grant, consoleOperations[op].permission);
}

/** 该操作是否为敏感操作（AUTH-19）：需要原因、Mfa-Assertion 与界面二次确认。 */
export function isSensitive(op: ConsoleOperation): boolean {
  return consoleOperations[op].sensitive;
}
