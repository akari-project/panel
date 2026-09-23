# migrations

规格：spec/03、spec/02 CONV-17–21。

- 已提交的迁移不可修改（Hook 拦截），改动一律新建 `NNNNN_描述.sql`。
- 加索引用 `CREATE INDEX CONCURRENTLY`，文件顶部 `-- +goose NO TRANSACTION`。
- 大表加非空列：可空列 → 回填 → 加约束，分多个迁移。
- 只追加表必须有禁止 UPDATE/DELETE 的触发器（CONV-18）。
- 内核协议矩阵随内核升级在新迁移中更新 `kernel_protocols`。
