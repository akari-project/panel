// SPDX-License-Identifier: AGPL-3.0-or-later
// RFC 9457 错误（CONV-16）的统一表示。界面依据 code 显示本地化文案，并显示 request_id（UI-02）。

export interface FieldError {
  field: string;
  code: string;
}

export interface Problem {
  status: number;
  code: string;
  title?: string;
  requestId?: string;
  errors: FieldError[];
  /** 429、503 等响应的 Retry-After（秒）；没有或无法解析时为 undefined。 */
  retryAfter?: number;
  /** 原始响应体，供 mfa_required 等携带附加字段的错误使用。 */
  body: Record<string, unknown>;
}

// 响应体不含 code 时（网关、Mock 或网络错误）按 HTTP 状态推断，保证界面总能取到一个已知的 code。
const codeByStatus: Record<number, string> = {
  400: 'invalid_request',
  401: 'unauthenticated',
  403: 'forbidden',
  404: 'not_found',
  409: 'conflict',
  413: 'payload_too_large',
  422: 'idempotency_key_reused',
  426: 'upgrade_required',
  428: 'precondition_required',
  429: 'rate_limited',
  503: 'service_unavailable',
};

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

export function toProblem(error: unknown, response?: Response): Problem {
  const status = response?.status ?? 0;
  const body = isRecord(error) ? error : {};
  const code =
    typeof body.code === 'string' ? body.code : (codeByStatus[status] ?? (status === 0 ? 'network' : 'internal'));
  const errors = Array.isArray(body.errors)
    ? body.errors.filter(
        (e): e is FieldError => isRecord(e) && typeof e.field === 'string' && typeof e.code === 'string',
      )
    : [];
  const retryAfterHeader = response?.headers.get('Retry-After');
  const retryAfter = retryAfterHeader && /^\d+$/.test(retryAfterHeader.trim()) ? Number(retryAfterHeader) : undefined;
  return {
    status,
    code,
    title: typeof body.title === 'string' ? body.title : undefined,
    requestId:
      typeof body.request_id === 'string' ? body.request_id : (response?.headers.get('X-Request-Id') ?? undefined),
    errors,
    ...(retryAfter !== undefined ? { retryAfter } : {}),
    body,
  };
}

/** 携带 Problem 的异常，供 TanStack Query 等以抛出方式处理错误的调用方使用。 */
export class ProblemError extends Error {
  readonly problem: Problem;
  constructor(problem: Problem) {
    super(problem.title ?? problem.code);
    this.name = 'ProblemError';
    this.problem = problem;
  }
}

export function isProblemError(e: unknown): e is ProblemError {
  return e instanceof ProblemError;
}

/**
 * 把 openapi-fetch 的结果转换为“成功返回数据，失败抛出 ProblemError”。
 * 网络错误（fetch 本身抛出）同样转换为 code 为 network 的 ProblemError。
 */
export async function unwrap<T>(
  request: Promise<{ data?: T; error?: unknown; response: Response }>,
): Promise<T> {
  let result: { data?: T; error?: unknown; response: Response };
  try {
    result = await request;
  } catch {
    throw new ProblemError(toProblem(undefined));
  }
  if (!result.response.ok) {
    throw new ProblemError(toProblem(result.error, result.response));
  }
  return result.data as T;
}
