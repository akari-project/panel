// SPDX-License-Identifier: AGPL-3.0-or-later

package testdb

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/akari-project/panel/server/internal/db/sqlc"
)

// InFlightRefresh 在事务中按刷新令牌轮换（AUTH-07）的加锁顺序锁住会话行（FOR UPDATE OF s），
// 等待另一个事务 run 在该会话行上阻塞后，再插入子会话（外键对账号行与设备行取 FOR KEY SHARE）并提交。
// 用于回归测试：持有账号行或设备行锁、再吊销会话的操作不得与进行中的刷新形成死锁。
// refreshHash 是被刷新会话的 refresh_token_hash。返回 run 的结果。
func InFlightRefresh(t *testing.T, pool *pgxpool.Pool, refreshHash string, run func() error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	q := sqlc.New(tx)
	sess, err := q.SessionByRefreshHash(ctx, refreshHash)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- run() }()
	// 等待 run 在会话行上阻塞（本数据库中出现等待锁的连接）。
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("operation finished without waiting for the session row: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := q.InsertSession(ctx, sqlc.InsertSessionParams{
		ID: uuid.New(), AccountID: sess.AccountID, DeviceID: sess.DeviceID, Audience: sess.Audience,
		RefreshTokenHash: uuid.NewString(), ParentID: &sess.ID, ExpiresAt: sess.ExpiresAt,
	}); err != nil {
		t.Fatalf("refresh blocked or aborted by the concurrent operation: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("refresh commit: %v", err)
	}
	return <-done
}
