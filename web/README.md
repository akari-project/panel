<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# panel/web

用户中心（`portal`）与管理后台（`admin`）的前端，pnpm workspace。规格见 spec/32、spec/41 41.2、spec/40 DEP-01–06。

| 包 | 内容 |
|---|---|
| `portal` | 用户中心，对接客户端接口 |
| `admin` | 管理后台，对接管理接口 |
| `ui` | 共享组件、主题、i18n、格式化、运行时配置 |
| `sdk` | 由 OpenAPI 生成的类型与 openapi-fetch 客户端 |

其余目录：`openapi/`（panel-spec 的固定版本拷贝）、`mock/`（Prism 与会话模拟网关）、`e2e/`（Playwright）、`tooling/`（共用 Vite 插件）、`scripts/`。

## 常用命令

需要 Node.js 22 或更高版本，以及 `packageManager` 字段中的 pnpm 版本（`corepack enable`）。

```sh
pnpm install
pnpm mock                # Prism :4010/:4011，会话模拟网关 :4000/:4001
pnpm dev:portal          # http://localhost:5173，/v1 代理到 :4000
pnpm dev:admin           # http://localhost:5174，/v1 代理到 :4001
pnpm check               # CI 中的全部检查（不含端到端）
pnpm -r build && pnpm e2e
```

Mock 中任意邮箱与密码都可以登录。用户中心的邮箱以 `mfa` 开头时要求二次验证；管理后台总是要求二次验证（AUTH-21），任意 6 位数字验证码或任意恢复码都可以通过。

## OpenAPI 与 SDK

`openapi/` 中是 panel-spec 某个 tag 的拷贝，`openapi/lock.json` 记录 tag、提交与 SHA-256。

```sh
pnpm sync-openapi --ref v0.2.0   # 从本地 panel-spec（PANEL_SPEC_DIR 或工作区中的仓库）或 GitHub 取指定 tag
pnpm gen                         # 重新生成 sdk/src/*.gen.ts
```

生成代码随提交入库。CI 运行 `pnpm gen:check`（拷贝被手改或生成结果过期时失败），随后的 `pnpm -r typecheck` 暴露接口变更导致的类型错误。

## 与控制面的嵌入约定（DEP-01–05、UI-06）

- 产物位于 `portal/dist`、`admin/dist`，资源全部为相对路径（`./assets/...`），带哈希的文件在 `assets/` 下，并有预压缩的 `.br` 与 `.gz`（`index.html` 除外）。
- `dist/build.json` 为 `{"commit": "<git 提交>"}`；提交取自环境变量 `PANEL_BUILD_COMMIT`，否则取 `git rev-parse HEAD`。
- `index.html` 的 `<head>` 中有占位注释 `<!--panel-config-->`。控制面返回 `index.html` 时：
  1. 把占位注释替换为 `<script nonce="{n}">window.__PANEL_CONFIG__={JSON}</script>`，JSON 中的 `<` 转义为 `<`；
  2. 把属性中以 `src="./`、`href="./` 开头的相对路径改为以 `base_path` 开头，使深层路径下的 SPA 回退页面也能加载资源（CSP 为 `base-uri 'none'`，不能使用 `<base>`）。
- `window.__PANEL_CONFIG__` 的字段：

| 字段 | 说明 |
|---|---|
| `site_name` | 站点名称 |
| `api_base_url` | 接口根地址，不含 `/v1`；空串表示与页面同源 |
| `source_url` | 页脚“源代码”链接（ARC-04） |
| `base_path` | 应用挂载路径，以 `/` 开头和结尾，例如 `/`、`/admin/` |
| `csp_nonce` | 与 CSP 头中相同的 nonce，供组件库动态插入的 `<style>` 使用 |

`mock/gateway.mjs` 的 `--serve-dist` 模式按以上约定模拟控制面（含 DEP-05 的安全头），端到端测试在此模式下运行。
