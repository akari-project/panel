// SPDX-License-Identifier: AGPL-3.0-or-later
// 金额、流量与时间的显示格式化（UI-04）。只做格式化，不做任何折算。

/** 金额：最小货币单位整数（CONV-05）按货币的小数位转为十进制字符串后交给 Intl，不经过浮点。 */
export function formatMoney(amountMinor: number | bigint, currency: string, locale: string): string {
  const nf = new Intl.NumberFormat(locale, { style: 'currency', currency });
  const digits = nf.resolvedOptions().maximumFractionDigits ?? 2;
  const n = BigInt(amountMinor);
  const neg = n < 0n;
  const abs = (neg ? -n : n).toString().padStart(digits + 1, '0');
  const int = abs.slice(0, abs.length - digits);
  const frac = digits > 0 ? `.${abs.slice(abs.length - digits)}` : '';
  // Intl.NumberFormat.format 接受十进制字符串（ECMA-402 NumberFormat v3），精度不受 double 限制。
  return nf.format(`${neg ? '-' : ''}${int}${frac}` as unknown as number);
}

const byteUnits = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'] as const;

/** 流量：字节数（CONV-07）按 1024 进位、以 IEC 单位显示，最多两位小数。 */
export function formatBytes(bytes: number | bigint, locale: string): string {
  let value = Number(bytes);
  let i = 0;
  while (Math.abs(value) >= 1024 && i < byteUnits.length - 1) {
    value /= 1024;
    i++;
  }
  const n = new Intl.NumberFormat(locale, { maximumFractionDigits: i === 0 ? 0 : 2 }).format(value);
  return `${n} ${byteUnits[i]}`;
}

/** 时间：RFC 3339 字符串按用户语言与时区显示（CONV-03）。 */
export function formatDateTime(value: string | Date, locale: string, timeZone?: string): string {
  const d = typeof value === 'string' ? new Date(value) : value;
  return new Intl.DateTimeFormat(locale, { dateStyle: 'medium', timeStyle: 'short', ...(timeZone ? { timeZone } : {}) }).format(d);
}
