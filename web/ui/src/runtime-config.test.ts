// SPDX-License-Identifier: AGPL-3.0-or-later
import { describe, expect, it } from 'vitest';
import { basePathFromModule, readPanelConfig } from './runtime-config';

describe('readPanelConfig', () => {
  it('未注入时使用默认值', () => {
    expect(readPanelConfig(undefined)).toEqual({
      app: '',
      site_name: 'Panel',
      api_base_url: '',
      source_url: '',
      source_revision: '',
      csp_nonce: '',
      base_path: '/',
    });
  });

  it('读取注入值', () => {
    const c = readPanelConfig(
      {
        app: 'admin',
        site_name: 'Akari',
        api_base_url: 'https://api.example.invalid',
        source_url: 'https://github.com/akari-project/panel/tree/abc',
        source_revision: 'abc',
        csp_nonce: 'n1',
      },
      'admin',
    );
    expect(c).toEqual({
      app: 'admin',
      site_name: 'Akari',
      api_base_url: 'https://api.example.invalid',
      source_url: 'https://github.com/akari-project/panel/tree/abc',
      source_revision: 'abc',
      csp_nonce: 'n1',
      base_path: '/admin/',
    });
  });

  it('忽略类型错误的字段', () => {
    expect(readPanelConfig({ site_name: 42, api_base_url: null })).toMatchObject({ site_name: 'Panel', api_base_url: '' });
  });

  it('默认读取 window.__PANEL_CONFIG__', () => {
    window.__PANEL_CONFIG__ = { site_name: 'FromWindow' };
    expect(readPanelConfig().site_name).toBe('FromWindow');
    delete window.__PANEL_CONFIG__;
  });
});

describe('basePathFromModule', () => {
  it('由入口脚本地址推出挂载路径', () => {
    expect(basePathFromModule('https://x.invalid/admin/assets/index-abc.js')).toBe('/admin/');
    expect(basePathFromModule('https://x.invalid/assets/index-abc.js')).toBe('/');
    expect(basePathFromModule('http://localhost:5173/src/main.tsx')).toBe('/');
  });
});
