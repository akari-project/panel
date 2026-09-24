// SPDX-License-Identifier: AGPL-3.0-or-later
import { describe, expect, it } from 'vitest';
import { readPanelConfig } from './runtime-config';

describe('readPanelConfig', () => {
  it('未注入时使用默认值', () => {
    expect(readPanelConfig(undefined)).toEqual({
      site_name: 'Panel',
      api_base_url: '',
      source_url: '',
      base_path: '/',
      csp_nonce: '',
    });
  });

  it('读取注入值并规范化 base_path', () => {
    const c = readPanelConfig({ site_name: 'Akari', api_base_url: 'https://api.example.invalid', base_path: 'admin', csp_nonce: 'n1' });
    expect(c).toMatchObject({ site_name: 'Akari', api_base_url: 'https://api.example.invalid', base_path: '/admin/', csp_nonce: 'n1' });
  });

  it('忽略类型错误的字段', () => {
    expect(readPanelConfig({ site_name: 42, base_path: null }).site_name).toBe('Panel');
  });

  it('默认读取 window.__PANEL_CONFIG__', () => {
    window.__PANEL_CONFIG__ = { site_name: 'FromWindow' };
    expect(readPanelConfig().site_name).toBe('FromWindow');
    delete window.__PANEL_CONFIG__;
  });
});
