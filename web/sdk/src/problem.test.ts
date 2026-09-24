// SPDX-License-Identifier: AGPL-3.0-or-later
import { describe, expect, it } from 'vitest';
import { ProblemError, toProblem, unwrap } from './problem';

describe('toProblem', () => {
  it('读取 problem+json 的字段', () => {
    const p = toProblem(
      { code: 'invalid_request', title: 'Bad Request', request_id: 'req_1', errors: [{ field: 'email', code: 'required' }, 'x'] },
      new Response(null, { status: 400 }),
    );
    expect(p).toMatchObject({ status: 400, code: 'invalid_request', requestId: 'req_1', errors: [{ field: 'email', code: 'required' }] });
  });

  it('响应体没有 code 时按状态推断', () => {
    expect(toProblem('oops', new Response(null, { status: 401 })).code).toBe('unauthenticated');
    expect(toProblem(undefined, new Response(null, { status: 502 })).code).toBe('internal');
    expect(toProblem(undefined).code).toBe('network');
  });
});

describe('unwrap', () => {
  it('成功时返回数据', async () => {
    await expect(unwrap(Promise.resolve({ data: 1, response: new Response(null, { status: 200 }) }))).resolves.toBe(1);
  });

  it('失败时抛出 ProblemError', async () => {
    const req = Promise.resolve({ error: { code: 'forbidden' }, response: new Response(null, { status: 403 }) });
    await expect(unwrap(req)).rejects.toBeInstanceOf(ProblemError);
  });

  it('网络错误转换为 network', async () => {
    await expect(unwrap(Promise.reject(new TypeError('fetch failed')))).rejects.toMatchObject({ problem: { code: 'network' } });
  });
});
