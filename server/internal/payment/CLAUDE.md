# payment：支付渠道

规格：spec/12 的 12.2–12.4。内置渠道：`alipayf2f`（支付宝当面付）、`testprovider`（仅 dev/test）。

## 必须遵守
- PAY-05 通知处理顺序：验签 → `app_id` → 订单存在 → 金额完全相等 → 状态 → 幂等标记 → 返回 `success`。
- PAY-06 验签在任何数据库写入之前；每条通知写入 `payment_notifications`。
- PAY-01 应用私钥只存密文，接口、日志、审计中永不出现。
- PAY-04 金额由分到元字符串只用整数运算。
- PAY-11 生产环境启用测试渠道时拒绝启动。

## 测试
- 使用 `testdata/alipay/` 中去敏的沙箱录制数据；覆盖 spec/12.6 列出的全部情形。
