# web

pnpm workspace：`portal`（用户中心）、`admin`（管理后台）、`ui`（共享组件）、`sdk`（从 OpenAPI 生成）。规格：spec/32（UI-01 至 UI-07）。

- 只通过 `@panel/sdk` 调用接口；后端未完成时 `pnpm mock`。
- 构建产物嵌入控制面：相对路径；运行时配置读 `window.__PANEL_CONFIG__`（UI-06）。
- 文案走 i18next（zh-CN、en）；深浅色；键盘可达；WCAG AA。
- 不在前端计算折算；报价明细来自接口。
- 完成前 `pnpm -r lint typecheck test build`；流程改动用 Playwright MCP 实际操作一遍。
