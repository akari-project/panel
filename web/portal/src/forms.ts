// SPDX-License-Identifier: AGPL-3.0-or-later
// 表单共用的校验规则。长度与格式与契约一致（AUTH-01、AUTH-03、AUTH-04）；最终以服务端校验为准。
import { useCallback, useEffect, useState } from 'react';
import { z } from 'zod';

export const PASSWORD_MIN = 8;
export const PASSWORD_MAX = 128;

// 长度按 Unicode 码点计算，与服务端一致。
const codePoints = (s: string) => [...s].length;

export const newPassword = z
  .string()
  .refine((s) => codePoints(s) >= PASSWORD_MIN, { error: 'validation.password_length' })
  .refine((s) => codePoints(s) <= PASSWORD_MAX, { error: 'validation.password_length' });

export const email = z.email({ error: 'validation.email' }).max(254, { error: 'validation.email' });

export const emailCode = z.string().regex(/^[0-9]{6}$/, { error: 'validation.email_code' });

export const totpCode = z.string().regex(/^[0-9]{6}$/, { error: 'validation.totp' });

/** 重置链接中的令牌：32 字节随机值的 base64url（43 个字符）。 */
export const RESET_TOKEN = /^[A-Za-z0-9_-]{43}$/;

/** 倒计时（秒）。用于“重新发送”按钮（AUTH-03 每分钟 1 次，或按 Retry-After）。 */
export function useCountdown() {
  const [until, setUntil] = useState(0);
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (until <= now) return;
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [until, now]);
  const remaining = Math.max(0, Math.ceil((until - now) / 1000));
  const start = useCallback((seconds: number) => {
    const t = Date.now();
    setNow(t);
    setUntil(t + seconds * 1000);
  }, []);
  return { remaining, start };
}
