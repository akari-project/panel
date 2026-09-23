# billing：套餐、权益、报价、订单

规格：spec/11、spec/12。开工前确认任务涉及的规则编号。

## 最常被违反的规则
- BIL-03 权益只能通过追加 `entitlement_events` 并在同一事务更新 `entitlements` 改变。
- BIL-06 续费、升降级只在 `active` / `over_quota` 时允许；免费账号只能新购。
- BIL-07 续费价格 = min(锁定价格, 当前价格)。
- BIL-11 剩余价值在 [0, 实付]，整数运算，最后一步向下取整（CONV-06）。
- BIL-13 没有宽限期；到期即 `ended`，回到免费账号。
- 续费不重置流量（spec/11 场景表）。
- ORD-05 开通前校验报价时的权益 `version`，不一致全额入余额（`stale_quote`）。
- ORD-06 有效期内创建、订单有效期内支付的续费订单按续费处理。

## 测试
- `make test-property`：rapid 性质测试（spec/11.7）。
- `adversarial_test.go` 由 billing-verifier 维护。
- 时间一律用注入的时钟。
