# export：第三方客户端配置导出

规格：spec/23（EXP-01 至 EXP-07）。路径 `GET /v1/configurations/{token}`。

- 适配器 `singbox`、`mihomo`、`base64` 共用同一节点中间表示。
- golden 文件只在确认输出变化符合预期时用 `-update` 更新。
- mihomo 适配器的 golden 数据与自研客户端共用（API-08）。
