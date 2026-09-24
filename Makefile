# SPDX-License-Identifier: AGPL-3.0-or-later
#
# panel 的构建与检查（spec/42 42.2）。本地与 CI 执行同一目标：make ci。
# 前端（web/）尚不存在时，前端相关步骤跳过并提示；make build 与 check-embed 需要前端。

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := ci

SERVER := server
WEB    := web
BIN    := bin

# 固定的工具版本。
SQLC_VERSION        := v1.31.1
STATICCHECK_VERSION := v0.8.1
GOVULNCHECK_VERSION := v1.8.0
GO_LICENSES_VERSION := v2.0.1
OAPI_CODEGEN_VERSION := v2.8.0

SQLC        := go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)
STATICCHECK := go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
OAPI_CODEGEN := go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$(OAPI_CODEGEN_VERSION)

# 构建信息写入二进制（spec/40 DEP-01）；嵌入产物的 build.json 记录同一提交。
# 前端构建以 PANEL_BUILD_COMMIT 写入各自的 build.json（web/README.md），与此处取同一个值。
COMMIT  ?= $(or $(PANEL_BUILD_COMMIT),$(shell git rev-parse HEAD 2>/dev/null))
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PKG_BUILDINFO := github.com/akari-project/panel/server/internal/buildinfo
LDFLAGS := -s -w -X $(PKG_BUILDINFO).Version=$(VERSION) -X $(PKG_BUILDINFO).Commit=$(COMMIT)

GENERATED := $(SERVER)/internal/db/sqlc $(SERVER)/internal/clientapi/gen

.PHONY: ci build build-noui web-build embed gen gen-sqlc gen-clientapi check-spec-version check-generated test test-property e2e e2e-portal conformance lint fmt-check vet staticcheck \
	check-clock check-spdx licenses vulncheck web-check check-embed clean

## 构建 ---------------------------------------------------------------

# 单一二进制，内含两个前端：pnpm -r build → 复制到 internal/webui/dist → go build（DEP-01）。
build: embed
	@mkdir -p $(BIN)
	cd $(SERVER) && CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o ../$(BIN)/panel ./cmd/panel
	@echo "build: $(BIN)/panel（commit $(COMMIT)，含前端）"

# 不含前端的二进制（DEP-06）。
build-noui:
	@mkdir -p $(BIN)
	cd $(SERVER) && CGO_ENABLED=0 go build -trimpath -tags noui -ldflags '$(LDFLAGS)' -o ../$(BIN)/panel-noui ./cmd/panel
	@echo "build-noui: $(BIN)/panel-noui"

web-build:
	@if [ ! -f $(WEB)/package.json ]; then echo "web-build: $(WEB)/package.json 不存在，无法构建前端（M0-06）"; exit 1; fi
	cd $(WEB) && pnpm install --frozen-lockfile && PANEL_BUILD_COMMIT=$(COMMIT) pnpm -r build

embed: web-build
	@if [ -z "$(COMMIT)" ]; then echo "embed: 无法取得 git 提交"; exit 1; fi
	cd $(SERVER) && go run ./tools/uiprep -web ../$(WEB) -out internal/webui/dist -commit $(COMMIT)

## 生成 ---------------------------------------------------------------

gen: gen-sqlc gen-clientapi

# sqlc：SQL 在 server/internal/db/queries，生成代码在 server/internal/db/sqlc（spec/41）。
gen-sqlc:
	cd $(SERVER) && $(SQLC) generate

# 客户端接口（spec/30）：契约取自 go.mod 锁定的 panel-spec 版本；tools/oapiprep 改写为 oapi-codegen 可处理的
# 3.0 形式并生成操作元数据，再由 oapi-codegen 生成 strict server，输出在 server/internal/clientapi/gen。
gen-clientapi:
	cd $(SERVER) && go mod download github.com/akari-project/panel-spec && \
	spec="$$(go list -m -f '{{.Dir}}' github.com/akari-project/panel-spec)/openapi/client/v1.yaml" && \
	tmp="$$(mktemp -d)" && trap 'rm -rf "$$tmp"' EXIT && \
	go run ./tools/oapiprep -spec "$$spec" -out "$$tmp/client.yaml" -meta internal/clientapi/gen/opmeta.gen.go && \
	$(OAPI_CODEGEN) -config internal/clientapi/oapi-codegen.yaml "$$tmp/client.yaml" && \
	{ printf '// SPDX-License-Identifier: AGPL-3.0-or-later\n'; cat internal/clientapi/gen/api.gen.go; } > "$$tmp/api.gen.go" && \
	mv "$$tmp/api.gen.go" internal/clientapi/gen/api.gen.go

# 后端（server/go.mod）与前端（web/openapi/lock.json）必须使用同一版本的 panel-spec 契约。
check-spec-version:
	@mod="$$(cd $(SERVER) && go list -m -f '{{.Version}}' github.com/akari-project/panel-spec)"; \
	web="$$(sed -n 's/.*"ref": *"\([^"]*\)".*/\1/p' $(WEB)/openapi/lock.json)"; \
	if [ "$$mod" != "$$web" ]; then echo "panel-spec 版本不一致：server/go.mod 为 $$mod，web/openapi/lock.json 为 $$web"; exit 1; fi; \
	echo "check-spec-version: $$mod"

# 生成物必须已提交且与源一致。
check-generated: gen check-spec-version
	git diff --exit-code -- $(GENERATED)
	@untracked="$$(git ls-files --others --exclude-standard -- $(GENERATED))"; \
	if [ -n "$$untracked" ]; then echo "未提交的生成文件："; echo "$$untracked"; exit 1; fi

## 测试 ---------------------------------------------------------------

# 单元测试与需要容器的集成测试（-race）。集成测试用 testcontainers 启动 PostgreSQL 18；
# 设置 PANEL_TEST_DATABASE_URL 可改用已有数据库，PANEL_REQUIRE_DB=1 时数据库不可用即失败（CI）。
# 端到端测试（server/e2e）只由 make e2e 运行。
test:
	cd $(SERVER) && go test -race $$(go list ./... | grep -v '/e2e/')
	cd $(SERVER) && go test -race -tags noui ./internal/webui/... ./internal/app/... ./cmd/...

# 性质测试（rapid），放在 property 构建标签下，不进入 make test（spec/42 42.3）。
test-property:
	cd $(SERVER) && go test -race -tags property -run '^TestProperty' ./...

# 端到端测试（spec/42 42.3）：模拟 Agent、测试用控制面端、协议一致性套件（含 1,000 节点规模测试）。
e2e:
	cd $(SERVER) && go test -race -count=1 -timeout 15m ./e2e/...

# 用户中心在真实控制面上的 Playwright 测试（M1-01 验收 4）：testcontainers 启动 PostgreSQL、Valkey、Mailpit，
# 进程内启动控制面，运行 web/playwright.real.config.ts。已设置 PLAYWRIGHT_BROWSERS_PATH 时不下载浏览器。
e2e-portal: web-build
	@if [ -z "$${PLAYWRIGHT_BROWSERS_PATH:-}" ]; then cd $(WEB) && pnpm exec playwright install --with-deps chromium; fi
	cd $(SERVER) && PANEL_E2E_PLAYWRIGHT=1 go test -count=1 -timeout 15m -run TestM1_01_PortalPlaywright ./e2e/portal/

# 协议一致性套件（spec/20 20.6）。对真实 Agent 运行：
#   make conformance CONFORMANCE_AGENT=exec CONFORMANCE_AGENT_CMD='...'（见 server/e2e/conformance/README.md）
conformance:
	cd $(SERVER) && go test -race -count=1 -timeout 30m -v ./e2e/conformance/...

## 检查 ---------------------------------------------------------------

lint: fmt-check vet staticcheck check-clock check-spdx

fmt-check:
	@out="$$(cd $(SERVER) && gofmt -l .)"; if [ -n "$$out" ]; then echo "未格式化的文件："; echo "$$out"; exit 1; fi

vet:
	cd $(SERVER) && go vet ./... && go vet -tags noui ./...

staticcheck:
	cd $(SERVER) && $(STATICCHECK) ./... && $(STATICCHECK) -tags noui ./...

# CONV-04：业务代码禁止直接调用 time.Now()，只有 internal/clock 可以。测试文件同样使用注入的时钟（ENG-05）。
check-clock:
	@hits="$$(grep -rn --include='*.go' -E '\btime\.(Now|Since|Until)\(' $(SERVER) | grep -v '^$(SERVER)/internal/clock/' || true)"; \
	if [ -n "$$hits" ]; then echo "禁止直接调用 time.Now/Since/Until（CONV-04），请使用 internal/clock："; echo "$$hits"; exit 1; fi; \
	echo "check-clock: 通过"

# 源文件前两行内必须有 SPDX 标识（CONV-25）。无法加头的文件（生成代码、锁文件）登记在 REUSE.toml 并在此排除；完整检查由 CI 的 REUSE lint 执行。
check-spdx:
	@missing="$$(find . -path ./.git -prune -o -path ./.worktrees -prune -o -path ./.claude -prune -o -path '*/node_modules' -prune -o -path ./$(SERVER)/internal/db/sqlc -prune -o -path ./$(WEB)/pnpm-lock.yaml -prune \
	  -o -path ./$(SERVER)/internal/webui/dist -prune -o -path '*/dist' -prune -o \
	  -type f \( -name '*.go' -o -name '*.sql' -o -name '*.yaml' -o -name '*.yml' -o -name '*.sh' -o -name '*.toml' -o -name Makefile \) -print \
	  | while read -r f; do head -n 2 "$$f" | grep -q 'SPDX-License-Identif[i]er:' || echo "$$f"; done)"; \
	if [ -n "$$missing" ]; then echo "缺少 SPDX 头："; echo "$$missing"; exit 1; fi; \
	echo "check-spdx: 通过"

# 依赖许可证扫描（spec/42 42.2，AGPL-3.0 仓库的允许清单）。本模块自身不参与判定。
ALLOWED_LICENSES := MIT,BSD-2-Clause,BSD-3-Clause,Apache-2.0,ISC,MPL-2.0,LGPL-2.1,LGPL-3.0,GPL-3.0,AGPL-3.0
# 人工核对过、但 go-licenses 无法自动识别的依赖：
#   github.com/oapi-codegen/nullable：LICENSE 为 Apache-2.0 的标准声明而非全文（v1.2.0 已核对）。
LICENSES_VERIFIED := github.com/oapi-codegen/nullable
licenses:
	cd $(SERVER) && go run github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION) check ./... \
	  --allowed_licenses=$(ALLOWED_LICENSES) --ignore github.com/akari-project/panel/server \
	  $(foreach m,$(LICENSES_VERIFIED),--ignore $(m))

vulncheck:
	cd $(SERVER) && go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

# 前端 lint / typecheck / test / build（spec/42 42.2）。
web-check:
	@if [ ! -f $(WEB)/package.json ]; then echo "web-check: $(WEB)/package.json 不存在，跳过（M0-06）"; exit 0; fi; \
	cd $(WEB) && pnpm install --frozen-lockfile && pnpm test:root && pnpm -r lint && pnpm -r typecheck && pnpm -r test && pnpm -r build

# 嵌入产物一致性（DEP-01）：make build 的二进制内嵌前端且提交一致；
# 用另一个提交构建的二进制必须拒绝启动。
check-embed:
	@if [ ! -f $(WEB)/package.json ]; then echo "check-embed: $(WEB)/package.json 不存在，跳过（M0-06）"; exit 0; fi; \
	$(MAKE) --no-print-directory build; \
	./$(BIN)/panel version | grep -q 'ui true' || { echo "check-embed: 二进制不含前端"; exit 1; }; \
	cd $(SERVER) && CGO_ENABLED=0 go build -ldflags '-X $(PKG_BUILDINFO).Commit=0000000000000000000000000000000000000000' -o ../$(BIN)/panel-mismatch ./cmd/panel; \
	cd ..; out="$$(PANEL_DATABASE_URL=postgres://unused/x ./$(BIN)/panel-mismatch api 2>&1 || true)"; \
	echo "$$out" | grep -q 'different commit' || { echo "check-embed: 提交不一致时没有拒绝启动："; echo "$$out"; exit 1; }; \
	rm -f $(BIN)/panel-mismatch; echo "check-embed: 通过"

## CI -----------------------------------------------------------------

ci: lint check-generated licenses test test-property e2e build-noui web-check check-embed e2e-portal

clean:
	rm -rf $(BIN)
	find $(SERVER)/internal/webui/dist -mindepth 1 ! -name .gitkeep -exec rm -rf {} +
