// SPDX-License-Identifier: AGPL-3.0-or-later
// 升级后旧页面引用的分块可能已不存在；分块加载失败时刷新 index.html（UI-06、DEP-03）。
// 为避免新版本同样失败时无限刷新，10 秒内只自动刷新一次。

const KEY = 'panel.chunk-reload-at';
const WINDOW_MS = 10_000;

export function installChunkReload(win: Window = window): void {
  win.addEventListener('vite:preloadError', (event) => {
    let last = 0;
    try {
      last = Number(win.sessionStorage.getItem(KEY) ?? 0);
    } catch {
      /* 存储不可用时仍然刷新一次 */
    }
    const now = Date.now();
    if (now - last < WINDOW_MS) return;
    event.preventDefault();
    try {
      win.sessionStorage.setItem(KEY, String(now));
    } catch {
      /* 忽略 */
    }
    win.location.reload();
  });
}
