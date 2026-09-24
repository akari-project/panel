// SPDX-License-Identifier: AGPL-3.0-or-later
// @panel/sdk：两份 OpenAPI 生成的类型与基于 openapi-fetch 的客户端（UI-01）。
import createClient from 'openapi-fetch';
import type { paths as ClientPaths, components as ClientComponents } from './client.gen';
import type { paths as ConsolePaths, components as ConsoleComponents } from './console.gen';
import { createSessionFetch, type SessionHooks } from './session';

export type { ClientPaths, ClientComponents, ConsolePaths, ConsoleComponents };
export type ClientSchemas = ClientComponents['schemas'];
export type ConsoleSchemas = ConsoleComponents['schemas'];

export * from './problem';
export type { SessionHooks } from './session';

export interface ApiOptions {
  /** 接口根地址，不含 /v1；空串表示与页面同源。来自运行时配置（UI-06）。 */
  baseUrl: string;
  fetch?: typeof globalThis.fetch;
}

// 浏览器使用 HttpOnly Cookie 认证（AUTH-08），跨源部署时同样需要携带 Cookie。
function options({ baseUrl, fetch }: ApiOptions) {
  return { baseUrl: baseUrl.replace(/\/+$/, ''), credentials: 'include' as const, ...(fetch ? { fetch } : {}) };
}

/**
 * 客户端接口（用户中心）。请求层自动处理访问令牌过期与重新验证（见 session.ts）；
 * 应用通过返回值的 `hooks` 提供重新验证框与会话失效时的处理。
 */
export function createClientApi(opts: ApiOptions) {
  const hooks: SessionHooks = {};
  const base = options(opts);
  // 不在创建时固定 globalThis.fetch，便于测试替换。
  const baseFetch = opts.fetch ?? ((input: RequestInfo | URL, init?: RequestInit) => globalThis.fetch(input, init));
  const client = createClient<ClientPaths>({
    ...base,
    fetch: createSessionFetch({ baseUrl: base.baseUrl, fetch: baseFetch, hooks }),
  });
  return Object.assign(client, { hooks });
}

/** 管理接口（管理后台）。 */
export function createConsoleApi(opts: ApiOptions) {
  return createClient<ConsolePaths>(options(opts));
}

export type ClientApi = ReturnType<typeof createClientApi>;
export type ConsoleApi = ReturnType<typeof createConsoleApi>;
