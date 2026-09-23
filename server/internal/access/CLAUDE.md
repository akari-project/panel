# access：权限协调器

规格：spec/11 的 ACS-01 至 ACS-06。

- 输入事件：`entitlement.changed`、`plan.access_changed`、`node.membership_changed`、`device.revoked`。
- 输出：对受影响节点下发 `CredUpsert` / `CredRemove`（spec/20），递增 `nodes.config_version`。
- 只下发 `active` 权益的凭据；`over_quota`、`suspended` 不下发（ACS-01）。
- 增量结果必须与全量重算一致（性质测试）；每晚全量对账（ACS-03）。
