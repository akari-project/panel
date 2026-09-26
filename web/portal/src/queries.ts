// SPDX-License-Identifier: AGPL-3.0-or-later
import { queryOptions } from '@tanstack/react-query';
import { unwrap, type ClientApi } from '@panel/sdk';

export const meQuery = (api: ClientApi) =>
  queryOptions({ queryKey: ['me'], queryFn: () => unwrap(api.GET('/v1/me')), staleTime: 60_000 });

export const devicesQuery = (api: ClientApi) => queryOptions({ queryKey: ['devices'], queryFn: () => unwrap(api.GET('/v1/me/devices')) });
