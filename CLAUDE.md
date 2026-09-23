# panel

控制面：Go 后端 `server/` + 前端 `web/`。AGPL-3.0-or-later，新文件第一行 `// SPDX-License-Identifier: AGPL-3.0-or-later`。
规格在 `../spec/`（从 workspace 启动时可见）。

## 命令
- `make build`：`pnpm -r build` → 复制到 `server/internal/webui/dist` → `go build`；`-tags noui` 不含前端
- `make gen`：从 panel-spec 生成接口代码；sqlc 生成数据访问代码
- `make test`：单元测试（含 -race）；`make test-property`：性质测试；`make e2e`：端到端；`make chaos`：计量混沌测试
- `make lint`；`cd web && pnpm -r lint typecheck test`；`pnpm mock`：按 OpenAPI 启动 Mock

## 目录与对应规格
| 目录 | 规格 |
|---|---|
| `server/cmd/panel` | 子命令 api / gateway / worker / all（spec/01） |
| `server/internal/auth`、`account`、`rbac` | spec/10 |
| `server/internal/billing` | spec/11、spec/12（有 CLAUDE.md） |
| `server/internal/payment` | spec/12（有 CLAUDE.md） |
| `server/internal/access` | spec/11 ACS（有 CLAUDE.md） |
| `server/internal/gateway` | spec/20（有 CLAUDE.md） |
| `server/internal/accounting` | spec/22（有 CLAUDE.md） |
| `server/internal/export` | spec/23（有 CLAUDE.md） |
| `server/internal/clientapi`、`consoleapi` | spec/30、spec/31 |
| `server/internal/notify`、`content`、`support`、`referral` | spec/13 |
| `server/internal/webui` | spec/40 DEP-01–06 |
| `server/migrations` | spec/03（有 CLAUDE.md） |
| `server/e2e/fakeagent`、`e2e/conformance` | 模拟 Agent 与协议一致性套件（spec/20） |
| `web/` | spec/32（有 CLAUDE.md） |

## 必须遵守
- 接口改动：先改 panel-spec 的 OpenAPI 或 proto，再 `make gen`，最后实现。不手写请求与响应类型。
- 生成代码与已提交的迁移不可手改（Hook 会拦截）。
- 时间只从注入的时钟获取（CONV-04）；金额只用整数（CONV-05）。
- 日志不输出令牌、凭据、完整 IP（CONV-24）。
