// SPDX-License-Identifier: AGPL-3.0-or-later
// portal 与 admin 共用的 Vite 插件，负责与控制面嵌入相关的构建约定（spec/40 DEP-01–06、spec/32 UI-06）：
// - 产物只使用相对路径（base './'）；
// - 控制面返回 index.html 时在 </head> 前插入带 nonce 的 window.__PANEL_CONFIG__ 脚本，并为已有的
//   <script>/<style> 补 nonce；开发服务器以同样的方式插入开发配置；
// - 主题初始化脚本以外部文件加载（CSP 不允许内联脚本）；
// - 构建后写 dist/build.json（git 提交），并为资源预压缩 br 与 gzip。
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { readdirSync, readFileSync, statSync, writeFileSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';
import { brotliCompressSync, constants as zc, gzipSync } from 'node:zlib';
import type { Plugin, UserConfig } from 'vite';

export interface PanelPluginOptions {
  /** 应用名，开发时写入运行时配置的 app */
  app: 'portal' | 'admin';
  /** 开发时本地 Mock 网关的地址，/v1 请求代理到这里（pnpm mock） */
  mockTarget: string;
  /** 开发服务器端口 */
  devPort: number;
  /** 开发时注入的站点名称 */
  siteName: string;
}

const themeInitPath = resolve(import.meta.dirname, '../ui/src/theme-init.js');
const compressible = /\.(js|mjs|css|html|svg|json|txt|map|webmanifest)$/;

function gitCommit(root: string): string {
  const fromEnv = process.env.PANEL_BUILD_COMMIT;
  if (fromEnv) return fromEnv;
  try {
    return execFileSync('git', ['rev-parse', 'HEAD'], { cwd: root, encoding: 'utf8' }).trim();
  } catch {
    return 'unknown';
  }
}

function walk(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const p = join(dir, name);
    return statSync(p).isDirectory() ? walk(p) : [p];
  });
}

export function panel(opts: PanelPluginOptions): Plugin {
  const themeInit = readFileSync(themeInitPath, 'utf8');
  const themeInitName = `assets/theme-init-${createHash('sha256').update(themeInit).digest('hex').slice(0, 8)}.js`;
  let root = process.cwd();
  let outDir = 'dist';
  let isBuild = false;

  return {
    name: 'panel',
    config(): UserConfig {
      return {
        base: './',
        build: {
          outDir: 'dist',
          emptyOutDir: true,
          sourcemap: false,
          assetsInlineLimit: 0,
          rolldownOptions: {
            output: {
              // 按依赖拆分，依赖升级时应用代码的分块哈希不变，反之亦然。
              codeSplitting: {
                groups: [
                  { name: 'react', test: /node_modules[\\/](react|react-dom|scheduler)[\\/]/ },
                  { name: 'tanstack', test: /node_modules[\\/]@tanstack[\\/]/ },
                  { name: 'vendor', test: /node_modules[\\/]/ },
                ],
              },
            },
          },
        },
        server: {
          port: opts.devPort,
          strictPort: true,
          proxy: { '/v1': { target: opts.mockTarget, changeOrigin: false } },
        },
        preview: { port: opts.devPort + 100, strictPort: true },
      };
    },
    configResolved(cfg) {
      root = cfg.root;
      outDir = resolve(cfg.root, cfg.build.outDir);
      isBuild = cfg.command === 'build';
    },
    transformIndexHtml(html) {
      if (isBuild) {
        return {
          html,
          tags: [{ tag: 'script', attrs: { src: `./${themeInitName}` }, injectTo: 'head' }],
        };
      }
      const devConfig = { app: opts.app, site_name: opts.siteName, api_base_url: '', source_url: '', source_revision: '', csp_nonce: '' };
      return html.replace(
        '</head>',
        `<script>${themeInit}</script><script>window.__PANEL_CONFIG__=${JSON.stringify(devConfig)}</script></head>`,
      );
    },
    generateBundle() {
      this.emitFile({ type: 'asset', fileName: themeInitName, source: themeInit });
    },
    closeBundle() {
      if (!isBuild) return;
      for (const file of walk(outDir)) {
        const rel = relative(outDir, file);
        // index.html 由控制面在返回时注入配置，预压缩版本无法使用。
        if (rel === 'index.html' || !compressible.test(file)) continue;
        const buf = readFileSync(file);
        writeFileSync(`${file}.br`, brotliCompressSync(buf, { params: { [zc.BROTLI_PARAM_QUALITY]: 11 } }));
        writeFileSync(`${file}.gz`, gzipSync(buf, { level: 9 }));
      }
      writeFileSync(join(outDir, 'build.json'), `${JSON.stringify({ commit: gitCommit(root) })}\n`);
    },
  };
}
