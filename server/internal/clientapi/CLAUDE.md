# clientapi：/v1 客户端接口

规格：spec/30、spec/13、spec/02。

- 处理器实现 oapi-codegen 生成的 strict server 接口，不手写请求与响应类型。
- `/v1` 冻结后只增可选字段。
- 错误一律 problem+json，`code` 取自 spec/02 错误表；新增错误码先改规格。
- 用户只能访问自己的资源；`not_found` 与无权访问不区分。
- 事件流（SSE）事件类型见 OpenAPI 的 `EventType`。
