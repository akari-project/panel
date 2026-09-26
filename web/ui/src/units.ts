// SPDX-License-Identifier: AGPL-3.0-or-later
// 输入层的单位换算：流量按 1024 进位的 IEC 单位（CONV-33），金额按货币的小数位转为最小单位（CONV-05）。
// 全部用字符串与 BigInt 运算，不经过浮点；换算回字节时向下取整（CONV-06）。这里不做任何计费折算。

export const byteInputUnits = { GiB: 1024n ** 3n, TiB: 1024n ** 4n } as const;
export type ByteInputUnit = keyof typeof byteInputUnits;

export type ParseResult = { ok: true; value: number } | { ok: false; code: 'invalid_format' | 'out_of_range' };

const decimal = /^(\d+)(?:\.(\d+))?$/;
const MAX = BigInt(Number.MAX_SAFE_INTEGER);

/** 把“数值 + 单位”（如 1.5 TiB）换算为字节数，向下取整。超过 JS 安全整数时返回 out_of_range。 */
export function parseBytesInput(text: string, unit: ByteInputUnit): ParseResult {
  const m = decimal.exec(text.trim());
  if (!m) return { ok: false, code: 'invalid_format' };
  const frac = m[2] ?? '';
  const scale = 10n ** BigInt(frac.length);
  const bytes = (BigInt(m[1]!) * scale + BigInt(frac || '0')) * byteInputUnits[unit] / scale;
  return bytes > MAX ? { ok: false, code: 'out_of_range' } : { ok: true, value: Number(bytes) };
}

/** 字节数转为输入框的初始值：整 TiB 时用 TiB，否则用 GiB；小数为精确值（2 的幂的倒数在十进制下有限）。 */
export function bytesToInput(bytes: number): { value: string; unit: ByteInputUnit } {
  const b = BigInt(bytes);
  const unit: ByteInputUnit = b > 0n && b % byteInputUnits.TiB === 0n ? 'TiB' : 'GiB';
  const d = byteInputUnits[unit];
  const int = b / d;
  let rem = b % d;
  let frac = '';
  while (rem > 0n) {
    rem *= 10n;
    frac += (rem / d).toString();
    rem %= d;
  }
  return { value: frac ? `${int}.${frac}` : int.toString(), unit };
}

/** 货币的小数位（如 CNY 为 2，JPY 为 0），取自 Intl。 */
export function currencyDigits(currency: string): number {
  return new Intl.NumberFormat('en', { style: 'currency', currency }).resolvedOptions().maximumFractionDigits ?? 2;
}

/** 把以主单位输入的金额（如 19.9）换算为最小单位整数（1990）。小数位多于货币允许的位数时返回 invalid_format。 */
export function parseMoneyInput(text: string, currency: string): ParseResult {
  const m = decimal.exec(text.trim());
  const digits = currencyDigits(currency);
  if (!m || (m[2]?.length ?? 0) > digits) return { ok: false, code: 'invalid_format' };
  const minor = BigInt(m[1]!) * 10n ** BigInt(digits) + BigInt((m[2] ?? '').padEnd(digits, '0') || '0');
  return minor > MAX ? { ok: false, code: 'out_of_range' } : { ok: true, value: Number(minor) };
}
