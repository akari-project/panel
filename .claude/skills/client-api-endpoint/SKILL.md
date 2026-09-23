---
name: client-api-endpoint
description: 新增或修改 /v1 客户端接口时使用。
---
# 新增 /v1 接口

1. 命名：按 spec/30“对外命名约定”与 spec/02 取名。当前用户的资源放在 `/v1/me/`；动作用子资源。不使用 subscribe、server、traffic、node。
2. 在 panel-spec 的 `openapi/client/v1.yaml` 定义（spec-change 流程）；错误码使用 spec/02 错误表中的值。
3. `make gen` 后实现 strict server 接口方法；不手写请求与响应类型。
4. 鉴权：默认需要登录；公开接口在 OpenAPI 中显式 `security: []`。
5. 限流：写接口按账号限流；公开接口按 IP 限流。
6. 幂等：有副作用的 POST 支持 `Idempotency-Key`。
7. 测试：成功、未登录、访问他人资源（应返回 404）、参数错误、限流。
8. 前端 SDK：`cd web && pnpm gen`，并在用到的页面上更新。
