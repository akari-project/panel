---
name: export-adapter
description: 为第三方客户端新增一种配置导出格式，或修改已有格式时使用。
---
# 新增第三方导出格式

1. 在 `server/internal/export/adapters/<名称>` 实现 `Adapter` 接口：输入统一节点中间表示与凭据，输出文本。
2. 在注册表中登记格式名与 User-Agent 识别规则。
3. golden 测试：`testdata/<名称>/*.golden`，覆盖全部 7 种协议、空配置（免费账号）、特殊字符节点名。
4. 加载校验：用对应客户端内核（或其解析库）加载生成的配置，确认无错误。
5. 更新 OpenAPI 中 `format` 参数的枚举（走 spec-change 流程）。
