// SPDX-License-Identifier: AGPL-3.0-or-later
// @panel/sdk：两份 OpenAPI 生成的类型与基于 openapi-fetch 的客户端（UI-01）。
import createClient from 'openapi-fetch';
import type { paths as ClientPaths, components as ClientComponents } from './client.gen';
import type { paths as ConsolePaths, components as ConsoleComponents } from './console.gen';

export type { ClientPaths, ClientComponents, ConsolePaths, ConsoleComponents };
export type ClientSchemas = ClientComponents['schemas'];
export type ConsoleSchemas = ConsoleComponents['schemas'];

export * from './problem';

export interface ApiOptions {
  /** 接口根地址，不含 /v1；空串表示与页面同源。来自运行时配置（UI-06）。 */
  baseUrl: string;
  fetch?: typeof globalThis.fetch;
}

// 浏览器使用 HttpOnly Cookie 认证（AUTH-08），跨源部署时同样需要携带 Cookie。
function options({ baseUrl, fetch }: ApiOptions) {
  return { baseUrl: baseUrl.replace(/\/+$/, ''), credentials: 'include' as const, ...(fetch ? { fetch } : {}) };
}

/** 客户端接口（用户中心）。 */
export function createClientApi(opts: ApiOptions) {
  return createClient<ClientPaths>(options(opts));
}

/** 管理接口（管理后台）。 */
export function createConsoleApi(opts: ApiOptions) {
  return createClient<ConsolePaths>(options(opts));
}

export type ClientApi = ReturnType<typeof createClientApi>;
export type ConsoleApi = ReturnType<typeof createConsoleApi>;
