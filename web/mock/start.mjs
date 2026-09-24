#!/usr/bin/env node
// SPDX-License-Identifier: AGPL-3.0-or-later
// pnpm mock：用 Prism 按两份 OpenAPI 的示例启动 Mock（UI-01），并在前面各放一个会话模拟网关。
//
//   客户端接口  Prism :4010  ← 网关 :4000  ← 用户中心开发服务器 :5173（/v1 代理）
//   管理接口    Prism :4011  ← 网关 :4001  ← 管理后台开发服务器 :5174（/v1 代理）
//
// 加 --serve-dist 时，另外启动 :4100（用户中心，挂载在 /）与 :4101、:4102（管理后台，挂载在 /admin/），
// 托管 portal/dist 与 admin/dist 并模拟控制面的注入，用于端到端测试。
import { spawn } from 'node:child_process';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createGateway } from './gateway.mjs';

const webDir = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const serveDist = process.argv.includes('--serve-dist');
const prismEntry = join(webDir, 'node_modules', '@stoplight', 'prism-cli', 'dist', 'index.js');

const apis = [
  { api: 'client', spec: 'client.v1.yaml', prismPort: 4010, port: 4000, authCookie: '__Host-access_token' },
  { api: 'console', spec: 'console.v1.yaml', prismPort: 4011, port: 4001, authCookie: '__Host-console_access_token' },
];
const apps = [
  { name: 'portal', api: 'client', port: 4100, basePath: '/', siteName: 'Akari' },
  { name: 'admin', api: 'console', port: 4101, basePath: '/admin/', siteName: 'Akari Console' },
  // 不改写相对路径的服务端（只在挂载路径与其下一级路径上可用），用于验证挂载路径由入口脚本地址推出。
  { name: 'admin', api: 'console', port: 4102, basePath: '/admin/', siteName: 'Akari Console', rewriteRelative: false },
];
const sourceUrl = 'https://github.com/akari-project/panel';
const revision = 'mock-revision';

const children = [];
function shutdown(code = 0) {
  for (const c of children) c.kill('SIGTERM');
  process.exit(code);
}
process.on('SIGINT', () => shutdown());
process.on('SIGTERM', () => shutdown());

function startPrism({ spec, prismPort }) {
  return new Promise((resolveReady, reject) => {
    // --multiprocess false：不派生工作进程，结束本进程时 Prism 随之退出。
    const args = ['mock', '--multiprocess', 'false', '--host', '127.0.0.1', '--port', String(prismPort), join(webDir, 'openapi', spec)];
    const child = spawn(process.execPath, [prismEntry, ...args], {
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    children.push(child);
    const onData = (buf) => {
      const text = buf.toString();
      if (process.env.MOCK_VERBOSE) process.stdout.write(`[prism ${spec}] ${text}`);
      if (text.includes('Prism is listening')) resolveReady();
    };
    child.stdout.on('data', onData);
    child.stderr.on('data', onData);
    child.on('exit', (code) => {
      console.error(`prism ${spec} 退出，退出码 ${code}；设置 MOCK_VERBOSE=1 查看输出`);
      reject(new Error('prism exited'));
      shutdown(1);
    });
  });
}

function listen(server, port) {
  return new Promise((r) => server.listen(port, '127.0.0.1', r));
}

await Promise.all(apis.map(startPrism));

for (const a of apis) {
  const upstream = `http://127.0.0.1:${a.prismPort}`;
  await listen(createGateway({ api: a.api, upstream, authCookie: a.authCookie }), a.port);
  console.log(`mock ${a.api} API: http://127.0.0.1:${a.port}（Prism ${upstream}）`);
  if (!serveDist) continue;
  for (const app of apps.filter((x) => x.api === a.api)) {
    const server = createGateway({
      api: a.api,
      upstream,
      authCookie: a.authCookie,
      app: {
        name: app.name,
        rewriteRelative: app.rewriteRelative,
        dir: join(webDir, app.name, 'dist'),
        basePath: app.basePath,
        config: { site_name: app.siteName, api_base_url: '', source_url: `${sourceUrl}/tree/${revision}`, source_revision: revision },
      },
    });
    await listen(server, app.port);
    console.log(`${app.name} dist: http://127.0.0.1:${app.port}${app.basePath}`);
  }
}
console.log('mock ready');
