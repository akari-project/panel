// SPDX-License-Identifier: AGPL-3.0-or-later
// RFC 6238 TOTP（SHA-1、30 秒、6 位），用于在测试中代替身份验证器。
import { createHmac } from 'node:crypto';

const STEP = 30;

function base32Decode(input: string): Buffer {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  const clean = input.replace(/[\s=]/g, '').toUpperCase();
  let bits = 0;
  let value = 0;
  const out: number[] = [];
  for (const ch of clean) {
    const i = alphabet.indexOf(ch);
    if (i < 0) throw new Error(`非法的 Base32 字符：${ch}`);
    value = (value << 5) | i;
    bits += 5;
    if (bits >= 8) {
      out.push((value >>> (bits - 8)) & 0xff);
      bits -= 8;
    }
  }
  return Buffer.from(out);
}

export function totpAt(secret: string, step: number): string {
  const counter = Buffer.alloc(8);
  counter.writeBigUInt64BE(BigInt(step));
  const mac = createHmac('sha1', base32Decode(secret)).update(counter).digest();
  const offset = mac[mac.length - 1]! & 0x0f;
  const bin = mac.readUInt32BE(offset) & 0x7fffffff;
  return String(bin % 1_000_000).padStart(6, '0');
}

export const currentStep = () => Math.floor(Date.now() / 1000 / STEP);

/**
 * 取一个时间步严格大于 usedStep 的验证码（同一时间步内已使用的验证码会被拒绝，AUTH-11）。
 * 离时间步结束不足 3 秒时也等到下一步，避免提交时跨过边界。
 */
export async function freshTotp(secret: string, usedStep = -1): Promise<{ code: string; step: number }> {
  for (;;) {
    const now = Date.now() / 1000;
    const step = Math.floor(now / STEP);
    const left = (step + 1) * STEP - now;
    if (step > usedStep && left > 3) return { code: totpAt(secret, step), step };
    await new Promise((r) => setTimeout(r, Math.ceil(left * 1000) + 100));
  }
}
