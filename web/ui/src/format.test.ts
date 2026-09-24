// SPDX-License-Identifier: AGPL-3.0-or-later
import { describe, expect, it } from 'vitest';
import { formatBytes, formatDateTime, formatMoney } from './format';

describe('formatMoney', () => {
  it('按货币小数位显示最小单位整数', () => {
    expect(formatMoney(1990, 'CNY', 'zh-CN')).toBe('¥19.90');
    expect(formatMoney(5, 'USD', 'en')).toBe('$0.05');
    expect(formatMoney(-1234, 'USD', 'en')).toBe('-$12.34');
    expect(formatMoney(1500, 'JPY', 'en')).toBe('¥1,500');
  });

  it('超过 2^53 的金额不丢精度', () => {
    expect(formatMoney(9007199254740993n, 'USD', 'en')).toBe('$90,071,992,547,409.93');
  });
});

describe('formatBytes', () => {
  it('按 1024 进位', () => {
    expect(formatBytes(512, 'en')).toBe('512 B');
    expect(formatBytes(1536, 'en')).toBe('1.5 KiB');
    expect(formatBytes(100n * 1024n ** 3n, 'zh-CN')).toBe('100 GiB');
  });
});

describe('formatDateTime', () => {
  it('按指定时区显示', () => {
    expect(formatDateTime('2026-10-01T10:00:00+08:00', 'en', 'UTC')).toContain('2:00');
    expect(formatDateTime('2026-10-01T10:00:00+08:00', 'en', 'Asia/Shanghai')).toContain('10:00');
  });
});
