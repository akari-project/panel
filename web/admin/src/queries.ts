// SPDX-License-Identifier: AGPL-3.0-or-later
import { queryOptions } from '@tanstack/react-query';
import { unwrap, type ConsoleApi } from '@panel/sdk';

/** 当前管理员与权限，供菜单与按钮判断（spec/31）。 */
export const staffMeQuery = (api: ConsoleApi) =>
  queryOptions({ queryKey: ['staff', 'me'], queryFn: () => unwrap(api.GET('/v1/staff/me')), staleTime: 60_000 });
