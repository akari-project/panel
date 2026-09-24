// SPDX-License-Identifier: AGPL-3.0-or-later
// 运行时配置由控制面在返回 index.html 时，以带 nonce 的内联脚本插入 </head> 之前（DEP-04、UI-06），
// 不在构建时写死域名。开发服务器与 Mock 下使用默认值。

export interface PanelConfig {
  /** 当前应用：portal 或 admin */
  app: string;
  /** 站点名称 */
  site_name: string;
  /** 接口根地址，不含 /v1；空串表示与页面同源 */
  api_base_url: string;
  /** 源代码链接（ARC-04），指向当前运行版本 */
  source_url: string;
  /** 当前运行版本的提交 */
  source_revision: string;
  /** CSP nonce，动态插入 <style>/<script> 时使用 */
  csp_nonce: string;
  /**
   * 应用挂载路径，以 / 开头和结尾，例如 "/" 或 "/admin/"。
   * 服务端不注入，由入口脚本的地址推出（见 basePathFromModule）。
   */
  base_path: string;
}

declare global {
  interface Window {
    __PANEL_CONFIG__?: Record<string, unknown>;
  }
}

function str(v: unknown, fallback = ''): string {
  return typeof v === 'string' ? v : fallback;
}

function normalizeBasePath(p: string): string {
  let out = p.startsWith('/') ? p : `/${p}`;
  if (!out.endsWith('/')) out += '/';
  return out;
}

/**
 * 由入口模块的地址推出挂载路径：构建产物的入口在 {挂载路径}/assets/ 下，开发时在 /src/ 下，
 * 两种情况下上一级目录都是挂载路径。
 */
export function basePathFromModule(moduleUrl: string): string {
  return new URL('..', moduleUrl).pathname;
}

/** 读取并规范化运行时配置；缺失或类型错误的字段使用默认值，未注入时整体使用默认值。 */
export function readPanelConfig(
  source: unknown = typeof window === 'undefined' ? undefined : window.__PANEL_CONFIG__,
  basePath = '/',
): PanelConfig {
  const raw = typeof source === 'object' && source !== null ? (source as Record<string, unknown>) : {};
  return {
    app: str(raw.app),
    site_name: str(raw.site_name) || 'Panel',
    api_base_url: str(raw.api_base_url),
    source_url: str(raw.source_url),
    source_revision: str(raw.source_revision),
    csp_nonce: str(raw.csp_nonce),
    base_path: normalizeBasePath(basePath),
  };
}
