// SPDX-License-Identifier: AGPL-3.0-or-later
// 位于浏览器与 Prism 之间的开发用网关，只用于 Mock 与端到端测试，不进入产物。
//
// 1. 会话模拟：Prism 不保存状态，也不会下发浏览器可用的 Cookie（__Host- 前缀要求 HTTPS）。
//    登录成功（POST /v1/sessions 返回 201）后，网关下发一个本地 Cookie；之后的请求带有该 Cookie 时，
//    网关向 Prism 补上 OpenAPI 声明的认证 Cookie，Prism 的认证校验因此通过；没有时 Prism 返回 401。
//    登出（DELETE /v1/sessions/current 返回 204）时清除该 Cookie。
// 2. 两步登录：提交密码时，管理接口一律返回 401 mfa_required（AUTH-21）；客户端接口在邮箱以 mfa 开头时返回。
// 3. 客户端接口的其余状态（都保存在本地 Cookie 中，各浏览器上下文互不影响）：
//    - 访问令牌与刷新令牌分开模拟：删除 panel_mock_client_access 即模拟访问令牌过期，
//      POST /v1/oauth/token 在仍有会话时重新下发，否则返回 400 invalid_grant（AUTH-07）。
//    - 二次验证是否启用、邮箱是否已验证：登录邮箱以 mfa 开头时已启用，以 unverified 开头时未验证；
//      启用、停用 TOTP 与验证邮箱会更新状态，并改写 GET /v1/me 的对应字段。
//    - 重新验证（AUTH-23）：修改密码、开始绑定与停用 TOTP、重新生成恢复码等在 5 分钟内未重新验证时返回 401 mfa_required；
//      登录成功视为一次重新验证（删除 panel_mock_client_reauth 即模拟窗口过期）；
//      POST /v1/me/reauthentications 的密码为 wrong-password 时返回 400 incorrect。
//    - 特定输入返回错误示例：注册邮箱以 closed 开头 → 403 registration_closed；验证码 000000 → 400 invalid_code；
//      重新发送的邮箱以 limited 开头 → 429（Retry-After）。
//    - 设备（AUTH-14、AUTH-15）：移除的设备记在 Cookie 中，之后从 GET /v1/me/devices 中去掉，再次移除返回 404；
//      移除当前设备（is_current）等同登出，清除会话 Cookie。登录邮箱以 full 开头时设备上限改为 1（名额已满），GET /v1/me 的权益为 active。
// 4. 管理接口的其余状态（同样保存在本地 Cookie 中）：
//    - 当前管理员：登录邮箱以 super 开头为 superadmin，以 support 开头为 support，其余为 Prism 示例（operator）；
//      改写 GET /v1/staff/me 与登录响应中的 staff。
//    - 首次登录绑定 TOTP（AUTH-21）：登录邮箱包含 +new 时，第一步返回 totp_enrollment，第二步返回恢复码。
//    - POST /v1/staff/me/step-up 的验证码为 000000 时返回 400 incorrect；成功时断言的有效期改为 5 分钟后。
//    - 接受邀请：令牌 inv_new 未提交密码时返回 400 password required；inv_expired 返回 400 expired；
//      inv_staff 返回 409 invalid_state。
// 5. 可选地托管构建产物，并按 spec/40 DEP-02–05 模拟控制面：SPA 回退、注入 window.__PANEL_CONFIG__、
//    为已有脚本补 nonce、重写 index.html 中的相对路径、安全头与 CSP nonce。用于在嵌入前验证产物（M0-06 验收 4）。
import { randomBytes } from 'node:crypto';
import { existsSync, readFileSync, statSync } from 'node:fs';
import { createServer, request as httpRequest } from 'node:http';
import { extname, join, normalize, sep } from 'node:path';

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
 * @param {{name: string, dir: string, basePath: string, config: object, rewriteRelative?: boolean}} [o.app] 托管的构建产物
 */
export function createGateway({ api, upstream, authCookie, app }) {
  const sessionCookie = `panel_mock_${api}`;

  const accessCookie = `${sessionCookie}_access`;
  const mfaCookie = `${sessionCookie}_mfa`;
  const unverifiedCookie = `${sessionCookie}_unverified`;
  const reauthCookie = `${sessionCookie}_reauth`;
  const roleCookie = `${sessionCookie}_role`;
  const enrollCookie = `${sessionCookie}_enroll`;
  const removedCookie = `${sessionCookie}_removed`;
  const currentCookie = `${sessionCookie}_current`;
  const fullCookie = `${sessionCookie}_full`;
  const removedIds = (cookies) => (cookies[removedCookie] ? cookies[removedCookie].split('.') : []);
  const cookieAttrs = 'Path=/; HttpOnly; SameSite=Strict';
  const setFlag = (name, on, maxAge) =>
    on ? `${name}=1; ${cookieAttrs}${maxAge ? `; Max-Age=${maxAge}` : ''}` : `${name}=; ${cookieAttrs}; Max-Age=0`;

  // 需要重新验证的操作（spec/10 AUTH-23）。
  const stepUp = new Set(['PUT /v1/me/password', 'POST /v1/me/mfa/totp', 'DELETE /v1/me/mfa/totp', 'POST /v1/me/mfa/recovery-codes', 'DELETE /v1/me', 'POST /v1/me/export-link/rotation']);

  function refreshToken(cookies, res) {
    if (cookies[sessionCookie]) {
      res.writeHead(200, { 'content-type': 'application/json', 'cache-control': 'no-store', 'set-cookie': [setFlag(accessCookie, true)] });
      res.end(JSON.stringify({ token_type: 'Bearer', expires_in: 900 }));
    } else {
      res.writeHead(400, { 'content-type': 'application/json', 'cache-control': 'no-store' });
      res.end(JSON.stringify({ error: 'invalid_grant', error_description: 'mock: no session' }));
    }
  }

  const staffByRole = {
    superadmin: { roles: ['superadmin'], permissions: ['*'], is_superadmin: true },
    support: { roles: ['support'], permissions: ['accounts.read', 'orders.read', 'tickets.*'], is_superadmin: false },
  };

  function problem(res, status, body) {
    res.writeHead(status, { 'content-type': 'application/problem+json' });
    res.end(JSON.stringify({ type: 'about:blank', title: body.code, status, request_id: randomBytes(8).toString('hex'), ...body }));
  }

  /** 管理接口中由网关直接应答的请求；已应答时返回 true。 */
  function consoleShortcut(route, json, res) {
    if (route === 'POST /v1/staff/me/step-up' && json?.totp_code === '000000') {
      problem(res, 400, { code: 'invalid_request', errors: [{ field: 'totp_code', code: 'incorrect' }] });
      return true;
    }
    if (route === 'POST /v1/staff-invitations/acceptance') {
      if (json?.token === 'inv_new' && !json.password) {
        problem(res, 400, { code: 'invalid_request', errors: [{ field: 'password', code: 'required' }] });
        return true;
      }
      if (json?.token === 'inv_expired') {
        problem(res, 400, { code: 'invalid_request', errors: [{ field: 'token', code: 'expired' }] });
        return true;
      }
      if (json?.token === 'inv_staff') {
        problem(res, 409, { code: 'invalid_state' });
        return true;
      }
    }
    return false;
  }

  async function proxy(req, res) {
    const body = await readBody(req);
    const url = new URL(req.url, upstream);
    const route = `${req.method} ${url.pathname}`;
    const headers = { ...req.headers, host: url.host };
    delete headers.cookie;
    delete headers['content-length'];
    const cookies = readCookies(req.headers.cookie);
    const client = api === 'client';
    // 客户端接口另外模拟访问令牌的有效期；管理接口只看会话。
    const hasAccess = client ? cookies[sessionCookie] && cookies[accessCookie] : cookies[sessionCookie];
    if (hasAccess) headers.cookie = `${authCookie}=mock-access-token`;

    if (client && route === 'POST /v1/oauth/token') {
      refreshToken(cookies, res);
      return;
    }

    const json = parseJson(body);
    if (!client && consoleShortcut(route, json, res)) return;
    const isLogin = req.method === 'POST' && url.pathname === '/v1/sessions';
    let loginEmail;
    // 第二步（提交 challenge_id）只会出现在启用了二次验证的账号上。
    const mfaAccount = isLogin && (typeof json?.challenge_id === 'string' || (json?.email?.startsWith?.('mfa') ?? false));
    if (isLogin) {
      const hasPassword = json && typeof json.password === 'string';
      loginEmail = typeof json?.email === 'string' ? json.email : undefined;
      const wantsMfa = api === 'console' || (loginEmail?.startsWith('mfa') ?? false);
      if (hasPassword && wantsMfa) {
        headers.prefer = `code=401, example=${!client && loginEmail?.includes('+new') ? 'totp_enrollment' : 'mfa_required'}`;
      }
      else if (client) headers.prefer = 'code=201, example=web';
    }
    if (client && stepUp.has(route)) {
      // 这些操作的 401 有两个示例；未登录时必须是 unauthenticated，否则 Prism 取第一个（mfa_required）。
      if (!hasAccess) headers.prefer = 'code=401, example=unauthenticated';
      else if (!cookies[reauthCookie]) headers.prefer = 'code=401, example=mfa_required';
    }
    if (client && route === 'POST /v1/me/reauthentications' && json?.password === 'wrong-password') {
      headers.prefer = 'code=400, example=incorrect';
    }
    const removeDevice = client && req.method === 'DELETE' && url.pathname.match(/^\/v1\/me\/devices\/([^/]+)$/)?.[1];
    if (removeDevice && hasAccess && removedIds(cookies).includes(removeDevice)) {
      problem(res, 404, { code: 'not_found' });
      return;
    }
    if (client && route === 'POST /v1/accounts' && json?.email?.startsWith?.('closed')) headers.prefer = 'code=403';
    if (client && route === 'POST /v1/accounts/verification' && json?.code === '000000') {
      headers.prefer = 'code=400, example=invalid_code';
    }
    if (client && route === 'POST /v1/accounts/verification/resend' && json?.email?.startsWith?.('limited')) {
      headers.prefer = 'code=429';
    }

    const upstreamReq = httpRequest(url, { method: req.method, headers }, (up) => {
      const out = { ...up.headers };
      // 上游的 Set-Cookie 带 Secure 与 __Host- 前缀，在本地 HTTP 下不可用，一律由网关自行下发。
      delete out['set-cookie'];
      const status = up.statusCode ?? 502;
      const setCookies = [];
      if (isLogin && status === 201) {
        setCookies.push(`${sessionCookie}=1; ${cookieAttrs}`, setFlag(accessCookie, true));
        if (client) {
          // 以密码完成的登录视为一次重新验证，5 分钟内有效（AUTH-23）。
          setCookies.push(setFlag(reauthCookie, true, 300));
          setCookies.push(setFlag(mfaCookie, mfaAccount));
          setCookies.push(setFlag(unverifiedCookie, loginEmail?.startsWith('unverified') ?? false));
          setCookies.push(setFlag(fullCookie, loginEmail?.startsWith('full') ?? false));
          setCookies.push(`${removedCookie}=; ${cookieAttrs}; Max-Age=0`);
        }
      }
      if (!client && isLogin && status === 401 && loginEmail) {
        // 第一步记下登录邮箱决定的角色与是否首次绑定，供第二步与之后的请求使用。
        const role = loginEmail.startsWith('super') ? 'superadmin' : loginEmail.startsWith('support') ? 'support' : '';
        setCookies.push(`${roleCookie}=${role}; ${cookieAttrs}`, setFlag(enrollCookie, loginEmail.includes('+new')));
      }
      const signOut = () => {
        for (const name of [sessionCookie, accessCookie, mfaCookie, unverifiedCookie, reauthCookie, roleCookie, enrollCookie, fullCookie, currentCookie]) {
          setCookies.push(setFlag(name, false));
        }
      };
      if (route === 'DELETE /v1/sessions/current' && status === 204) signOut();
      if (removeDevice && status === 204) {
        setCookies.push(`${removedCookie}=${[...removedIds(cookies), removeDevice].join('.')}; ${cookieAttrs}`);
        if (removeDevice === cookies[currentCookie]) signOut();
      }
      if (client && status < 300) {
        if (route === 'POST /v1/me/reauthentications') setCookies.push(setFlag(reauthCookie, true, 300));
        if (route === 'POST /v1/me/mfa/totp/activation') setCookies.push(setFlag(mfaCookie, true));
        if (route === 'DELETE /v1/me/mfa/totp') setCookies.push(setFlag(mfaCookie, false));
        if (route === 'POST /v1/accounts/verification' && cookies[sessionCookie]) setCookies.push(setFlag(unverifiedCookie, false));
      }
      if (setCookies.length) out['set-cookie'] = setCookies;

      // Prism 示例的断言有效期是固定的过去时刻；改为 5 分钟后，界面才会复用断言（AUTH-19）。
      if (!client && route === 'POST /v1/staff/me/step-up' && status === 201) {
        const chunks = [];
        up.on('data', (c) => chunks.push(c));
        up.on('end', () => {
          const data = parseJson(Buffer.concat(chunks)) ?? {};
          delete out['content-length'];
          delete out['transfer-encoding'];
          res.writeHead(status, out);
          res.end(JSON.stringify({ ...data, expires_at: new Date(Date.now() + 300_000).toISOString() }));
        });
        return;
      }

      // 管理接口：按本地状态改写当前管理员，首次绑定时登录响应附恢复码。
      const staffOverride = staffByRole[cookies[roleCookie]];
      const rewriteStaff = !client && status < 300 && (route === 'GET /v1/staff/me' || (isLogin && status === 201));
      if (rewriteStaff && (staffOverride || (isLogin && cookies[enrollCookie]))) {
        const chunks = [];
        up.on('data', (c) => chunks.push(c));
        up.on('end', () => {
          const data = parseJson(Buffer.concat(chunks)) ?? {};
          let next = data;
          if (route === 'GET /v1/staff/me') next = { ...data, ...staffOverride };
          else {
            next = { ...data, staff: { ...data.staff, ...staffOverride } };
            if (cookies[enrollCookie]) {
              next.recovery_codes = Array.from({ length: 10 }, (_, i) => `mock-${String(i).padStart(4, '0')}-code`);
              setCookies.push(setFlag(enrollCookie, false));
              out['set-cookie'] = setCookies;
            }
          }
          delete out['content-length'];
          delete out['transfer-encoding'];
          res.writeHead(status, out);
          res.end(JSON.stringify(next));
        });
        return;
      }

      // GET /v1/me/devices：去掉已移除的设备，记下当前设备；名额已满的账号上限改为 1，另有一台等待凭据的非 web 设备。
      if (client && route === 'GET /v1/me/devices' && status === 200) {
        const chunks = [];
        up.on('data', (c) => chunks.push(c));
        up.on('end', () => {
          const data = parseJson(Buffer.concat(chunks)) ?? {};
          const removed = removedIds(cookies);
          const items = (data.items ?? []).filter((d) => !removed.includes(d.id));
          const current = items.find((d) => d.is_current);
          if (current) setCookies.push(`${currentCookie}=${current.id}; ${cookieAttrs}`);
          if (setCookies.length) out['set-cookie'] = setCookies;
          delete out['content-length'];
          delete out['transfer-encoding'];
          res.writeHead(status, out);
          if (cookies[fullCookie]) {
            items.push({
              id: '0192f0c4-4a00-7000-8000-00000000f0ff',
              platform: 'android',
              model: 'Pixel 9',
              app_version: '1.4.0',
              created_at: '2026-10-01T09:30:00+08:00',
              last_seen_at: '2026-10-01T09:30:00+08:00',
              ip_prefix: '203.0.113.0/24',
              is_current: false,
              has_credential: false,
            });
          }
          res.end(JSON.stringify({ ...data, items, ...(cookies[fullCookie] ? { device_limit: 1 } : {}) }));
        });
        return;
      }

      // GET /v1/me：按本地状态改写二次验证、邮箱验证字段与名额已满账号的权益。
      if (client && route === 'GET /v1/me' && status === 200) {
        const chunks = [];
        up.on('data', (c) => chunks.push(c));
        up.on('end', () => {
          const me = parseJson(Buffer.concat(chunks)) ?? {};
          const mfa = !!cookies[mfaCookie];
          const text = JSON.stringify({
            ...me,
            is_mfa_enabled: mfa,
            mfa_methods: mfa ? ['totp', 'recovery_code'] : [],
            is_email_verified: !cookies[unverifiedCookie],
            // 名额已满的账号有生效中的权益：只有权益生效时，未获得凭据的设备才是名额不足（AUTH-14）。
            ...(cookies[fullCookie] ? { entitlement_status: 'active', device_limit: 1 } : {}),
          });
          delete out['content-length'];
          delete out['transfer-encoding'];
          res.writeHead(status, out);
          res.end(text);
        });
        return;
      }
      res.writeHead(status, out);
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
    const config = { app: app.name, ...app.config, csp_nonce: nonce };
    const json = JSON.stringify(config).replace(/</g, '\\u003c');
    let html = readFileSync(indexPath, 'utf8')
      // 与 M0-05 约定：为已有的 <script>/<style> 补 nonce，并在 </head> 前插入配置脚本。
      .replace(/<(script|style)(?=[\s>])/g, `<$1 nonce="${nonce}"`)
      .replace('</head>', `<script nonce="${nonce}">window.__PANEL_CONFIG__=${json}</script></head>`);
    // 待定（见 README）：深层路径的 SPA 回退页面中，相对路径会相对当前路径解析。
    // 这里按提议把 src="./、href="./ 改为挂载路径；rewriteRelative 为 false 时模拟不改写的服务端。
    if (app.rewriteRelative !== false) html = html.replace(/(src|href)="\.\//g, `$1="${app.basePath}`);
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
