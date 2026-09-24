// SPDX-License-Identifier: AGPL-3.0-or-later
// 用户中心的会话恢复（请求层）：
// - 访问令牌过期（401 unauthenticated）时，用 Cookie 中的刷新令牌调用一次 POST /v1/oauth/token，成功后重试原请求；
//   失败时通知应用回到登录页（AUTH-07、AUTH-08）。同一标签页内并发请求共用一次刷新；
//   多个标签页之间用 Web Locks 保证同一时间只有一个刷新请求（UI-08）。
// - 需要重新验证的操作返回 401 mfa_required 时，交给应用弹出验证框，完成后自动重试原请求（AUTH-23、UI-09）。
import createClient from 'openapi-fetch';
import type { paths as ClientPaths } from './client.gen';
import { toProblem, type Problem } from './problem';

export interface SessionHooks {
  /** 弹出重新验证框。验证成功返回 true（随后重试原请求），用户取消返回 false。 */
  reauthenticate?: (problem: Problem) => Promise<boolean>;
  /** 刷新令牌无效或已过期，会话无法恢复。 */
  sessionExpired?: () => void;
}

// 这些接口的 401 表示凭据不正确或登录需要第二步，不是令牌过期，也不是重新验证（AUTH-20）。
const noRecovery = new Set(['/v1/sessions', '/v1/oauth/token']);

const LOCK_NAME = 'panel.session-refresh';
const REFRESHED_AT_KEY = 'panel.session-refreshed-at';

function readRefreshedAt(): number {
  try {
    return Number(globalThis.localStorage?.getItem(REFRESHED_AT_KEY) ?? 0) || 0;
  } catch {
    return 0;
  }
}

function writeRefreshedAt(t: number) {
  try {
    globalThis.localStorage?.setItem(REFRESHED_AT_KEY, String(t));
  } catch {
    // 存储不可用（隐私模式等）时只失去跨标签页的去重优化。
  }
}

async function withCrossTabLock<T>(fn: () => Promise<T>): Promise<T> {
  const locks = (globalThis.navigator as Navigator | undefined)?.locks;
  if (!locks?.request) return fn();
  return locks.request(LOCK_NAME, fn);
}

async function problemOf(res: Response): Promise<Problem | null> {
  if (res.status !== 401) return null;
  try {
    return toProblem(await res.clone().json(), res);
  } catch {
    return toProblem(undefined, res);
  }
}

export interface SessionFetchOptions {
  baseUrl: string;
  fetch: typeof globalThis.fetch;
  hooks: SessionHooks;
}

/** 返回带会话恢复的 fetch，供 openapi-fetch 使用。 */
export function createSessionFetch({ baseUrl, fetch: baseFetch, hooks }: SessionFetchOptions): typeof globalThis.fetch {
  // 刷新令牌只用于这一个请求，不经过恢复逻辑，避免递归。
  const raw = createClient<ClientPaths>({ baseUrl, credentials: 'include', fetch: baseFetch });
  let refreshing: Promise<boolean> | null = null;
  let reauthenticating: Promise<boolean> | null = null;

  const refresh = (startedAt: number): Promise<boolean> => {
    refreshing ??= withCrossTabLock(async () => {
      // 等锁期间其他标签页已刷新过（Cookie 已是新令牌）时，直接重试原请求。
      if (readRefreshedAt() > startedAt) return true;
      try {
        const { response } = await raw.POST('/v1/oauth/token', {
          body: { grant_type: 'refresh_token' },
          headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        });
        if (response.ok) writeRefreshedAt(Date.now());
        return response.ok;
      } catch {
        return false;
      }
    }).finally(() => {
      refreshing = null;
    });
    return refreshing;
  };

  const reauthenticate = (p: Problem): Promise<boolean> => {
    if (!hooks.reauthenticate) return Promise.resolve(false);
    reauthenticating ??= hooks.reauthenticate(p).finally(() => {
      reauthenticating = null;
    });
    return reauthenticating;
  };

  return async (input, init) => {
    const request = new Request(input, init);
    // 请求体只能读取一次：每次发送都用副本，原请求留作重试。
    const replay = () => request.clone();
    const startedAt = Date.now();
    const first = await baseFetch(replay());
    // 接口根地址可能带路径前缀，只比较 /v1/ 之后的部分。
    const apiPath = new URL(request.url).pathname.replace(/^.*?(?=\/v1\/)/, '');
    if (noRecovery.has(apiPath)) return first;

    let res = first;
    let p = await problemOf(res);
    if (p?.code === 'unauthenticated') {
      if (!(await refresh(startedAt))) {
        hooks.sessionExpired?.();
        return res;
      }
      res = await baseFetch(replay());
      p = await problemOf(res);
    }
    if (p?.code === 'mfa_required' && (await reauthenticate(p))) {
      res = await baseFetch(replay());
    }
    return res;
  };
}
