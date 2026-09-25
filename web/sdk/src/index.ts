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
export { sessionKeys, type SessionHooks, type ReauthResult } from './session';
export * from './permissions';

export interface ApiOptions {
  /** 接口根地址，不含 /v1；空串表示与页面同源。来自运行时配置（UI-06）。 */
  baseUrl: string;
  fetch?: typeof globalThis.fetch;
}

// 浏览器使用 HttpOnly Cookie 认证（AUTH-08），跨源部署时同样需要携带 Cookie。
function options({ baseUrl, fetch }: ApiOptions) {
  return { baseUrl: baseUrl.replace(/\/+$/, ''), credentials: 'include' as const, ...(fetch ? { fetch } : {}) };
}

/** 带会话恢复的 fetch 与设置 hooks 的方法（见 session.ts）。 */
function sessionFetch(opts: ApiOptions, baseUrl: string, namespace?: string) {
  const hooks: SessionHooks = {};
  // 不在创建时固定 globalThis.fetch，便于测试替换。
  const baseFetch = opts.fetch ?? ((input: RequestInfo | URL, init?: RequestInit) => globalThis.fetch(input, init));
  const setSessionHooks = (next: SessionHooks) => {
    Object.assign(hooks, next);
    return () => {
      for (const k of Object.keys(next) as (keyof SessionHooks)[]) {
        if (hooks[k] === next[k]) delete hooks[k];
      }
    };
  };
  return { fetch: createSessionFetch({ baseUrl, fetch: baseFetch, hooks, namespace }), setSessionHooks };
}

/**
 * 客户端接口（用户中心）。请求层自动处理访问令牌过期与重新验证（见 session.ts）；
 * 应用通过 `setSessionHooks` 提供重新验证框与会话失效时的处理，返回值用于撤销。
 */
export function createClientApi(opts: ApiOptions) {
  const base = options(opts);
  const { fetch, setSessionHooks } = sessionFetch(opts, base.baseUrl);
  return Object.assign(createClient<ClientPaths>({ ...base, fetch }), { setSessionHooks });
}

/**
 * 管理接口（管理后台）。会话恢复同客户端接口，跨标签页的锁与时间戳使用独立的命名空间；
 * 敏感操作的 Mfa-Assertion 由应用经 `setSessionHooks` 提供（AUTH-19）。
 */
export function createConsoleApi(opts: ApiOptions) {
  const base = options(opts);
  const { fetch, setSessionHooks } = sessionFetch(opts, base.baseUrl, 'console');
  return Object.assign(createClient<ConsolePaths>({ ...base, fetch }), { setSessionHooks });
}

export type ClientApi = ReturnType<typeof createClientApi>;
export type ConsoleApi = ReturnType<typeof createConsoleApi>;
