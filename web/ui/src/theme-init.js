// SPDX-License-Identifier: AGPL-3.0-or-later
// 在首次绘制前应用主题，避免闪烁。以外部脚本加载，满足 CSP script-src 'self'（DEP-05）。
(function () {
  var t = null;
  try {
    t = localStorage.getItem('panel.theme');
  } catch {
    /* 存储不可用时跟随系统 */
  }
  var dark = t === 'dark' || (t !== 'light' && window.matchMedia('(prefers-color-scheme: dark)').matches);
  document.documentElement.classList.toggle('dark', dark);
})();
