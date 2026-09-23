---
name: billing-change
description: 修改套餐、价格、报价、订单、折算、权益状态机或到期处理时使用。
---
# 修改计费逻辑

1. 阅读 `server/internal/billing/CLAUDE.md` 与 spec/11、spec/12，列出本次改动涉及的规则编号。
2. 若改动影响规则本身，先改规格与 ADR，再改代码。
3. 新增权益变化类型时：在 `entitlement_events.type` 的 CHECK 中新增值（新迁移）、在状态机表中补全转换、在权限协调器中确认会触发重算。
4. 测试：
   - 为改动补充 rapid 性质测试；`make test-property`。
   - 时间相关逻辑用假时钟覆盖临界点：到期前 1 秒、到期瞬间、到期后 1 秒；周期切换瞬间。
   - 调用 `billing-verifier` 子代理尝试构造反例。
5. 前端：报价明细字段有变化时同步更新用户中心的明细展示。
6. 用户可见的规则变化同步更新帮助文档与到期提醒文案。
