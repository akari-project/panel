---
name: accounting-change
description: 修改流量计量、上报、Valkey 入账脚本、落库或分区维护时使用。
---
# 修改流量链路

1. 阅读 `server/internal/accounting/CLAUDE.md` 与 spec/22 的 ACC 规则。
2. Lua 脚本修改后，为脚本编写独立测试（真实 Valkey 容器），覆盖去重、倍率、超额判定。
3. 落库逻辑修改后，确认批次幂等：同一批次重复执行不改变结果。
4. 运行 `make chaos`：随机断线、重启网关与 worker、Valkey 重启，最终 PostgreSQL 用量等于节点计数总和。
5. 涉及 Agent 端时，与 agent-dev 协作，WAL 格式变更须兼容旧文件。
