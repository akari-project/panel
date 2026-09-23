---
name: payment-provider
description: 修改内置支付宝当面付，或新增一个内置支付渠道时使用。
---
# 支付渠道

规格：spec/12。代码：`server/internal/payment/`。

## 修改支付宝当面付
1. 阅读 `server/internal/payment/CLAUDE.md` 与 spec/12 的 PAY 规则。
2. 签名与验签：RSA2（SHA256）；公钥模式与证书模式二选一；验签必须在任何数据库写入之前。
3. 金额：`amount_minor` → 两位小数字符串只用整数运算；通知中的 `total_amount` 必须与订单应付完全一致。
4. 幂等：以 `(provider, trade_no)` 为键；重复通知返回 `success` 且不重复开通。
5. 测试：使用 `testdata/alipay/` 中录制的沙箱请求与通知（已去敏）；覆盖验签失败、`app_id` 不符、金额不符、重复通知、过期到账、查询兜底、关闭与退款。
6. 调用 security-reviewer 审查。

## 新增内置渠道
1. 在 `server/internal/payment/<渠道>` 实现 `PaymentProvider`：`Create`、`VerifyNotification`、`Query`、`Close`、`Refund`、`TestConnection`。
2. 在 `panel-spec` 的管理接口中增加该渠道的设置资源（spec-change 流程），私钥类字段只写不读。
3. 后台“支付设置”页增加表单与“测试连接”。
4. 同样的测试要求。
