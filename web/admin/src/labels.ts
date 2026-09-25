// SPDX-License-Identifier: AGPL-3.0-or-later
// 角色、权限与审计动作的本地化名称。未知取值（自定义角色、新增的动作）显示原始标识。
import { useTranslation } from 'react-i18next';

export const builtinRoles = ['superadmin', 'operator', 'support'] as const;

/** 权限标识含 `.` 与 `*`，转换为 i18n 键：`plans.*` → `plans_all`。 */
function permissionKey(p: string) {
  return p.replace(/\*/g, 'all').replace(/[.-]/g, '_');
}

export function useLabels() {
  const { t } = useTranslation();
  return {
    role: (name: string) => ((builtinRoles as readonly string[]).includes(name) ? t(`roles.builtin.${name}`) : name),
    permission: (p: string) => t(`permissions.${permissionKey(p)}`, { defaultValue: p }),
    // 审计动作目录见 spec/31 CON-09。
    action: (a: string) => t(`audit.action.${a}`, { defaultValue: a }),
  };
}
