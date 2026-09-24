// SPDX-License-Identifier: AGPL-3.0-or-later
// 运行时配置由控制面在返回 index.html 时注入（DEP-04、UI-06），不在构建时写死域名。

export interface PanelConfig {
  /** 站点名称 */
  site_name: string;
  /** 接口根地址，不含 /v1；空串表示与页面同源 */
  api_base_url: string;
  /** 源代码链接（ARC-04） */
  source_url: string;
  /** 应用挂载路径，以 / 开头和结尾，例如 "/" 或 "/admin/" */
  base_path: string;
  /** CSP nonce，动态插入 <style>/<script> 时使用 */
  csp_nonce: string;
}

declare global {
  interface Window {
    __PANEL_CONFIG__?: Partial<PanelConfig>;
  }
}

const defaults: PanelConfig = {
  site_name: 'Panel',
  api_base_url: '',
  source_url: '',
  base_path: '/',
  csp_nonce: '',
};

function str(v: unknown, fallback: string): string {
  return typeof v === 'string' ? v : fallback;
}

function normalizeBasePath(p: string): string {
  let out = p.startsWith('/') ? p : `/${p}`;
  if (!out.endsWith('/')) out += '/';
  return out;
}

/** 读取并规范化运行时配置；缺失字段使用默认值，未注入时整体使用默认值。 */
export function readPanelConfig(source: unknown = typeof window === 'undefined' ? undefined : window.__PANEL_CONFIG__): PanelConfig {
  const raw = typeof source === 'object' && source !== null ? (source as Record<string, unknown>) : {};
  return {
    site_name: str(raw.site_name, defaults.site_name) || defaults.site_name,
    api_base_url: str(raw.api_base_url, defaults.api_base_url),
    source_url: str(raw.source_url, defaults.source_url),
    base_path: normalizeBasePath(str(raw.base_path, defaults.base_path)),
    csp_nonce: str(raw.csp_nonce, defaults.csp_nonce),
  };
}
