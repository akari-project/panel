// SPDX-License-Identifier: AGPL-3.0-or-later
// CSP 的 style-src 只允许 'self' 与 nonce（DEP-05）。Radix 的滚动锁会动态插入 <style>，
// 通过 get-nonce 为其设置运行时配置中的 nonce，否则会被 CSP 拦截。
import { setNonce } from 'get-nonce';

export function applyCspNonce(nonce: string): void {
  if (nonce) setNonce(nonce);
}
