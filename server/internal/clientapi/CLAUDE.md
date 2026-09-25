# clientapi：/v1 客户端接口

规格：spec/30、spec/13、spec/02。

- 处理器实现 oapi-codegen 生成的 strict server 接口，不手写请求与响应类型。
- 实现新操作：把 operationId 加入 `oapi-codegen.yaml` 的 `include-operation-ids`，`make gen`，再实现 `gen.StrictServerInterface` 新增的方法。未列入的操作返回 404。
- 认证方式、是否接受 `Idempotency-Key`、是否检查客户端最低版本（响应列出 426，spec/30 API-03）由契约决定（`gen.Operations`，tools/oapiprep 生成），不在代码中另列清单。处理器返回 `*apierr.Error` 表示错误。
- 接受 `Idempotency-Key` 的操作，其处理器在返回 401 或 429 之前不得提交副作用（写库、outbox、通知）：这两类响应不缓存，客户端会用同一键重试（CONV-12，见 `internal/idempotency` 包注释）。测试断言返回 401/429 时没有写库。
- 唯一的手写响应类型：`GET /v1/config` 的 `signedConfigResponse` 原样写出规范化的签名文档（形状仍是契约的 `SignedConfig`），使响应字节、签名与 ETag 一致（API-11）。
- `/v1` 冻结后只增可选字段。
- 错误一律 problem+json，`code` 取自 spec/02 错误表；新增错误码先改规格。
- 用户只能访问自己的资源；`not_found` 与无权访问不区分。
- 事件流（SSE）事件类型见 OpenAPI 的 `EventType`。
