<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# panel/server

控制面后端，单一二进制 `panel`（spec/01 1.1）。

## 命令

| 命令 | 说明 |
|---|---|
| `panel migrate [status]` | 执行嵌入的 goose 迁移，或查看状态。迁移只由此命令执行；其他角色启动时只校验数据库版本（spec/40 DEP-12） |
| `panel api` / `gateway` / `worker` | 分别启动一个角色 |
| `panel all` | 在同一进程中启动全部角色，一个端口按 Host 把 `gateway.hosts` 分给网关（单机部署） |
| `panel admin create --email <邮箱> [--password-stdin]` | 创建首个超级管理员；已有超级管理员时拒绝，其他管理员经邀请加入（spec/10 AUTH-21、AUTH-22） |
| `panel version` | 版本、提交、是否内含前端 |

配置见 `panel.example.yaml`，由 `--config` 或 `PANEL_CONFIG` 指定，环境变量覆盖其中字段（`internal/config`）。

## 前端嵌入（spec/40 DEP-01–06）

- `make build`：`pnpm -r build` → `tools/uiprep` 复制到 `internal/webui/dist/{portal,admin}`、预压缩、写 `build.json` → `go build`（`-ldflags` 写入同一提交）。提交不一致时拒绝启动。
- 不带 `noui` 标签但没有嵌入前端的二进制同样拒绝启动；开发时用 `-tags noui` 加 Vite 开发服务器。
- **`-tags noui` 部署**：运营者自行托管的前端必须保留页脚的“源代码”链接，指向正在运行的版本的源码（AGPL，spec/01 ARC-04）。

## 测试

- `make test`：`go test -race`。集成测试用 testcontainers 启动 PostgreSQL 18（需要 Docker）；`PANEL_TEST_DATABASE_URL` 可改用已有的 PostgreSQL 18；没有数据库时集成测试跳过，`PANEL_REQUIRE_DB=1` 时改为失败（CI）。
- 时间一律经 `internal/clock` 注入（CONV-04），`make lint` 中的 `check-clock` 检查。
