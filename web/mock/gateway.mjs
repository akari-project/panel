// SPDX-License-Identifier: AGPL-3.0-or-later
// 位于浏览器与 Prism 之间的开发用网关，只用于 Mock 与端到端测试，不进入产物。
//
// 1. 会话模拟：Prism 不保存状态，也不会下发浏览器可用的 Cookie（__Host- 前缀要求 HTTPS）。
//    登录成功（POST /v1/sessions 返回 201）后，网关下发一个本地 Cookie；之后的请求带有该 Cookie 时，
//    网关向 Prism 补上 OpenAPI 声明的认证 Cookie，Prism 的认证校验因此通过；没有时 Prism 返回 401。
//    登出（DELETE /v1/sessions/current 返回 204）时清除该 Cookie。
// 2. 两步登录：提交密码时，管理接口一律返回 401 mfa_required（AUTH-21）；客户端接口在邮箱以 mfa 开头时返回。
// 3. 可选地托管构建产物，并按 spec/40 DEP-02–05 模拟控制面：SPA 回退、注入 window.__PANEL_CONFIG__、
//    重写 index.html 中的相对路径、安全头与 CSP nonce。用于在嵌入前验证产物（M0-06 验收 4）。
import { randomBytes } from 'node:crypto';
import { existsSync, readFileSync, statSync } from 'node:fs';
import { createServer, request as httpRequest } from 'node:http';
import { extname, join, normalize, sep } from 'node:path';

const CONFIG_PLACEHOLDER = '<!--panel-config-->';

const mimeTypes = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.woff2': 'font/woff2',
};

function readCookies(header = '') {
  return Object.fromEntries(
    header
      .split(';')
      .map((p) => p.trim().split('='))
      .filter(([k]) => k),
  );
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function parseJson(buf) {
  try {
    return JSON.parse(buf.toString('utf8'));
  } catch {
    return undefined;
  }
}

/**
 * @param {object} o
 * @param {'client'|'console'} o.api
 * @param {string} o.upstream           Prism 地址，例如 http://127.0.0.1:4010
 * @param {string} o.authCookie         OpenAPI 声明的认证 Cookie 名
 * @param {{dir: string, basePath: string, config: object}} [o.app] 托管的构建产物
 */
export function createGateway({ api, upstream, authCookie, app }) {
  const sessionCookie = `panel_mock_${api}`;

  async function proxy(req, res) {
    const body = await readBody(req);
    const url = new URL(req.url, upstream);
    const headers = { ...req.headers, host: url.host };
    delete headers.cookie;
    delete headers['content-length'];
    const cookies = readCookies(req.headers.cookie);
    if (cookies[sessionCookie]) headers.cookie = `${authCookie}=mock-access-token`;

    const isLogin = req.method === 'POST' && url.pathname === '/v1/sessions';
    if (isLogin) {
      const json = parseJson(body);
      const hasPassword = json && typeof json.password === 'string';
      const wantsMfa = api === 'console' || (typeof json?.email === 'string' && json.email.startsWith('mfa'));
      if (hasPassword && wantsMfa) headers.prefer = 'code=401, example=mfa_required';
      else if (api === 'client') headers.prefer = 'code=201, example=web';
    }

    const upstreamReq = httpRequest(url, { method: req.method, headers }, (up) => {
      const out = { ...up.headers };
      // 上游的 Set-Cookie 带 Secure 与 __Host- 前缀，在本地 HTTP 下不可用，一律由网关自行下发。
      delete out['set-cookie'];
      const setCookies = [];
      if (isLogin && up.statusCode === 201) {
        setCookies.push(`${sessionCookie}=1; Path=/; HttpOnly; SameSite=Strict`);
      }
      if (req.method === 'DELETE' && url.pathname === '/v1/sessions/current' && up.statusCode === 204) {
        setCookies.push(`${sessionCookie}=; Path=/; Max-Age=0; HttpOnly; SameSite=Strict`);
      }
      if (setCookies.length) out['set-cookie'] = setCookies;
      res.writeHead(up.statusCode ?? 502, out);
      up.pipe(res);
    });
    upstreamReq.on('error', (err) => {
      res.writeHead(502, { 'content-type': 'application/problem+json' });
      res.end(JSON.stringify({ type: 'about:blank', title: 'Mock upstream unavailable', status: 502, code: 'internal', detail: String(err) }));
    });
    upstreamReq.end(body);
  }

  function serveIndex(res) {
    const indexPath = join(app.dir, 'index.html');
    if (!existsSync(indexPath)) {
      res.writeHead(503, { 'content-type': 'text/plain; charset=utf-8' }).end(`${indexPath} 不存在，请先运行 pnpm build`);
      return;
    }
    const nonce = randomBytes(16).toString('base64');
    const config = { ...app.config, base_path: app.basePath, csp_nonce: nonce };
    const json = JSON.stringify(config).replace(/</g, '\\u003c');
    const html = readFileSync(indexPath, 'utf8')
      .replace(CONFIG_PLACEHOLDER, `<script nonce="${nonce}">window.__PANEL_CONFIG__=${json}</script>`)
      .replace(/(src|href)="\.\//g, `$1="${app.basePath}`);
    res.writeHead(200, {
      'content-type': mimeTypes['.html'],
      'cache-control': 'no-cache',
      'content-security-policy': [
        "default-src 'self'",
        `script-src 'self' 'nonce-${nonce}'`,
        `style-src 'self' 'nonce-${nonce}'`,
        "img-src 'self' data:",
        "connect-src 'self'",
        "frame-ancestors 'none'",
        "base-uri 'none'",
        "form-action 'self'",
      ].join('; '),
      'x-frame-options': 'DENY',
      'x-content-type-options': 'nosniff',
      'referrer-policy': 'strict-origin-when-cross-origin',
    });
    res.end(html);
  }

  function serveStatic(req, res) {
    const url = new URL(req.url, 'http://local');
    if (!url.pathname.startsWith(app.basePath)) {
      res.writeHead(404).end();
      return;
    }
    const rel = normalize(decodeURIComponent(url.pathname.slice(app.basePath.length)));
    const file = join(app.dir, rel);
    const isFile = rel !== '.' && !rel.startsWith('..') && file.startsWith(app.dir + sep) && existsSync(file) && statSync(file).isFile();
    if (!isFile || rel === 'index.html') {
      serveIndex(res);
      return;
    }
    res.writeHead(200, {
      'content-type': mimeTypes[extname(file)] ?? 'application/octet-stream',
      'cache-control': rel.startsWith(`assets${sep}`) ? 'public, max-age=31536000, immutable' : 'no-cache',
      'x-content-type-options': 'nosniff',
    });
    res.end(readFileSync(file));
  }

  return createServer((req, res) => {
    const path = new URL(req.url, 'http://local').pathname;
    if (path.startsWith('/v1/')) {
      proxy(req, res).catch((err) => {
        res.writeHead(500).end(String(err));
      });
    } else if (app) {
      serveStatic(req, res);
    } else {
      res.writeHead(404).end();
    }
  });
}
