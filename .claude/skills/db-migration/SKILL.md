---
name: db-migration
description: 新增或修改数据库表、列、索引、约束、触发器时使用。
---
# 编写迁移

1. 新建 `server/migrations/NNNNN_描述.sql`（编号取当前最大值 + 1），包含 `-- +goose Up` 与 `-- +goose Down`。
2. 遵守 spec/02：复数表名、`_id` 外键、`created_at`、`_enc` 加密列、`_hash` 摘要列。
3. 在线安全：
   - 加索引用 `CREATE INDEX CONCURRENTLY`，迁移顶部加 `-- +goose NO TRANSACTION`。
   - 加非空列：先可空 → 回填 → 加 `NOT NULL`，拆成多个迁移。
   - 不在迁移中做大表全量 UPDATE；改由 worker 分批回填。
4. 更新 `server/sql/queries/*.sql` 并运行 `make gen`（sqlc）。
5. 测试：`make test` 会在容器中从零执行全部迁移；为新约束或触发器写专门测试。
6. PR 描述中注明：是否可在线执行、预计锁时间、回滚方式。
