// SPDX-License-Identifier: AGPL-3.0-or-later
import { describe, expect, it } from 'vitest';
import { bytesToInput, currencyDigits, parseBytesInput, parseMoneyInput } from './units';

describe('parseBytesInput（CONV-33）', () => {
  it('按 1024 进位换算为字节', () => {
    expect(parseBytesInput('100', 'GiB')).toEqual({ ok: true, value: 107374182400 });
    expect(parseBytesInput('1', 'TiB')).toEqual({ ok: true, value: 1099511627776 });
    expect(parseBytesInput('1.5', 'TiB')).toEqual({ ok: true, value: 1649267441664 });
    expect(parseBytesInput(' 0 ', 'GiB')).toEqual({ ok: true, value: 0 });
  });

  it('不能整除时向下取整（CONV-06）', () => {
    // 0.3 GiB = 322122547.2 字节
    expect(parseBytesInput('0.3', 'GiB')).toEqual({ ok: true, value: 322122547 });
    expect(parseBytesInput('0.000000000001', 'GiB')).toEqual({ ok: true, value: 0 });
  });

  it('格式错误与越界', () => {
    for (const s of ['', '-1', '1e3', '1,5', '1.', '.5', 'abc']) expect(parseBytesInput(s, 'GiB')).toEqual({ ok: false, code: 'invalid_format' });
    expect(parseBytesInput('8192', 'TiB')).toEqual({ ok: false, code: 'out_of_range' });
    expect(parseBytesInput('8191', 'TiB').ok).toBe(true);
  });
});

describe('bytesToInput', () => {
  it('整 TiB 用 TiB，其余用 GiB，并可无损换算回来', () => {
    expect(bytesToInput(1099511627776)).toEqual({ value: '1', unit: 'TiB' });
    expect(bytesToInput(214748364800)).toEqual({ value: '200', unit: 'GiB' });
    expect(bytesToInput(0)).toEqual({ value: '0', unit: 'GiB' });
    expect(bytesToInput(536870912)).toEqual({ value: '0.5', unit: 'GiB' });
    for (const b of [1, 322122547, 1649267441664, 123456789012345]) {
      const { value, unit } = bytesToInput(b);
      expect(parseBytesInput(value, unit)).toEqual({ ok: true, value: b });
    }
  });
});

describe('parseMoneyInput（CONV-05）', () => {
  it('按货币小数位换算为最小单位', () => {
    expect(currencyDigits('CNY')).toBe(2);
    expect(currencyDigits('JPY')).toBe(0);
    expect(parseMoneyInput('19.9', 'CNY')).toEqual({ ok: true, value: 1990 });
    expect(parseMoneyInput('30', 'CNY')).toEqual({ ok: true, value: 3000 });
    expect(parseMoneyInput('0.07', 'USD')).toEqual({ ok: true, value: 7 });
    expect(parseMoneyInput('1500', 'JPY')).toEqual({ ok: true, value: 1500 });
  });

  it('小数位过多或格式错误时拒绝，不做舍入', () => {
    expect(parseMoneyInput('19.999', 'CNY')).toEqual({ ok: false, code: 'invalid_format' });
    expect(parseMoneyInput('1.5', 'JPY')).toEqual({ ok: false, code: 'invalid_format' });
    expect(parseMoneyInput('', 'CNY')).toEqual({ ok: false, code: 'invalid_format' });
    expect(parseMoneyInput('99999999999999999', 'CNY')).toEqual({ ok: false, code: 'out_of_range' });
  });
});
