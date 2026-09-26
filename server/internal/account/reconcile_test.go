// SPDX-License-Identifier: AGPL-3.0-or-later

package account

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/secretbox"
	"github.com/akari-project/panel/server/internal/testdb"
)

var rt0 = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

type reconcileEnv struct {
	pool   *pgxpool.Pool
	keys   *secretbox.Keyring
	acct   uuid.UUID
	shared uuid.UUID
}

func newReconcileEnv(t *testing.T) *reconcileEnv {
	t.Helper()
	keys, err := secretbox.ParseKeyring("1:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	e := &reconcileEnv{pool: testdb.New(t), keys: keys}
	ctx := context.Background()
	if err := e.pool.QueryRow(ctx, `INSERT INTO accounts (email, referral_code) VALUES ('r@example.com', 'RRRRRRRR') RETURNING id`).Scan(&e.acct); err != nil {
		t.Fatal(err)
	}
	e.shared, err = CreateSharedCredential(ctx, sqlc.New(e.pool), keys, e.acct)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// entitle 设置账号的当前权益（快照 device_limit）。
func (e *reconcileEnv) entitle(t *testing.T, status string, limit int) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `DELETE FROM entitlements WHERE account_id = $1`, e.acct); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `
		WITH p AS (INSERT INTO plans (name, tier, bytes_per_cycle, device_limit) VALUES ($4, 1, 0, $3) RETURNING id)
		INSERT INTO entitlements (account_id, plan_id, status, starts_at, cycle_start, bytes_limit, device_limit, reset_policy)
		SELECT $1, p.id, $2, $5, $5, 0, $3, 'never' FROM p`, e.acct, status, limit, uuid.NewString(), rt0); err != nil {
		t.Fatal(err)
	}
}

// device 插入一台设备；seen 为 nil 表示从未活跃。
func (e *reconcileEnv) device(t *testing.T, platform string, seen *time.Time) uuid.UUID {
	t.Helper()
	id, err := sqlc.New(e.pool).InsertDevice(context.Background(), sqlc.InsertDeviceParams{AccountID: e.acct, Platform: platform, Now: seen})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *reconcileEnv) reconcile(t *testing.T) {
	t.Helper()
	err := pgx.BeginFunc(context.Background(), e.pool, func(tx pgx.Tx) error {
		return ReconcileCredentials(context.Background(), sqlc.New(tx), e.keys, e.acct, rt0)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// holders 返回持有未吊销凭据的设备。
func (e *reconcileEnv) holders(t *testing.T) []uuid.UUID {
	t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT device_id FROM proxy_credentials WHERE account_id = $1 AND device_id IS NOT NULL AND revoked_at IS NULL ORDER BY device_id`, e.acct)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// events 返回 credential.changed 事件中的 change 取值（按写入顺序），并核对载荷（CONV-34）。
func (e *reconcileEnv) events(t *testing.T) []string {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT payload FROM outbox WHERE topic = 'credential.changed' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	payloads, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, b := range payloads {
		var p struct {
			SchemaVersion int       `json:"schema_version"`
			AccountID     uuid.UUID `json:"account_id"`
			CredentialID  uuid.UUID `json:"credential_id"`
			Change        string    `json:"change"`
		}
		if err := json.Unmarshal(b, &p); err != nil || p.SchemaVersion != 1 || p.AccountID != e.acct || p.CredentialID == uuid.Nil {
			t.Fatalf("payload %s", b)
		}
		out = append(out, p.Change)
	}
	return out
}

func (e *reconcileEnv) sharedIntact(t *testing.T) {
	t.Helper()
	var revoked *time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT revoked_at FROM proxy_credentials WHERE id = $1`, e.shared).Scan(&revoked); err != nil || revoked != nil {
		t.Fatalf("shared credential touched: %v %v", revoked, err)
	}
}

func sorted(ids ...uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return out
}

func at(m int) *time.Time {
	t := rt0.Add(time.Duration(m) * time.Minute)
	return &t
}

// AUTH-14：权益 active 时按 last_seen_at 从近到远填满空闲名额；web 设备不占名额；共用凭据不受影响。
func TestReconcileFillsByRecency(t *testing.T) {
	e := newReconcileEnv(t)
	old := e.device(t, "ios", at(1))
	never := e.device(t, "android", nil)
	recent := e.device(t, "macos", at(5))
	mid := e.device(t, "linux", at(3))
	e.device(t, "web", at(9))

	// 没有权益：不生成凭据。
	e.reconcile(t)
	if h := e.holders(t); len(h) != 0 {
		t.Fatalf("credentials without entitlement: %v", h)
	}
	e.entitle(t, "active", 2)
	e.reconcile(t)
	if h := e.holders(t); !slices.Equal(h, sorted(recent, mid)) {
		t.Fatalf("holders = %v, want recent and mid", h)
	}
	// 幂等：再次调用不产生变化。
	e.reconcile(t)
	e.entitle(t, "active", 4)
	e.reconcile(t)
	if h := e.holders(t); !slices.Equal(h, sorted(recent, mid, old, never)) {
		t.Fatalf("holders = %v, want all four", h)
	}
	if ev := e.events(t); !slices.Equal(ev, []string{"created", "created", "created", "created", "created"}) {
		t.Fatalf("events = %v", ev) // 第一条是共用凭据
	}
	e.sharedIntact(t)
}

// AUTH-14：上限降低时按 last_seen_at 从远到近吊销凭据，保留设备记录；新设备不抢占已持有凭据的设备。
func TestReconcileOverLimit(t *testing.T) {
	e := newReconcileEnv(t)
	e.entitle(t, "active", 3)
	a := e.device(t, "ios", at(1))
	b := e.device(t, "ios", at(2))
	c := e.device(t, "ios", at(3))
	e.reconcile(t)
	if h := e.holders(t); !slices.Equal(h, sorted(a, b, c)) {
		t.Fatalf("holders = %v", h)
	}
	// 更近活跃的新设备只等待，不抢占。
	d := e.device(t, "ios", at(10))
	e.reconcile(t)
	if h := e.holders(t); !slices.Equal(h, sorted(a, b, c)) {
		t.Fatalf("new device preempted: %v", h)
	}
	// 上限降为 1：持有凭据的设备中保留最近活跃的 c，吊销 a、b；等待中的 d 仍然等待。
	e.entitle(t, "active", 1)
	e.reconcile(t)
	if h := e.holders(t); !slices.Equal(h, sorted(c)) || slices.Contains(h, d) {
		t.Fatalf("holders after downgrade = %v, want c", h)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM devices WHERE account_id = $1 AND revoked_at IS NULL`, e.acct); n != 4 {
		t.Fatalf("device records removed: %d left", n)
	}
	if ev := e.events(t); !slices.Equal(ev, []string{"created", "created", "created", "created", "revoked", "revoked"}) {
		t.Fatalf("events = %v", ev)
	}
	e.sharedIntact(t)
}

// BIL-13、ACS-02：权益不是 active 时不吊销也不生成；持有数超出上限时仍按上限收回。
func TestReconcileInactiveEntitlement(t *testing.T) {
	e := newReconcileEnv(t)
	e.entitle(t, "active", 2)
	a := e.device(t, "ios", at(1))
	b := e.device(t, "ios", at(2))
	e.reconcile(t)
	for _, st := range []string{"over_quota", "suspended"} {
		e.entitle(t, st, 2)
		e.device(t, "ios", at(5))
		e.reconcile(t)
		if h := e.holders(t); !slices.Equal(h, sorted(a, b)) {
			t.Fatalf("%s: holders = %v", st, h)
		}
		status, err := DeviceCredentialStatus(context.Background(), sqlc.New(e.pool), e.acct, a)
		if err != nil || status != StatusEntitlementInactive {
			t.Fatalf("%s: status of a credentialed device = %s %v, want entitlement_inactive", st, status, err)
		}
	}
	// 权益结束：没有当前权益时同样不吊销（到期由 BIL-13 处理）。
	if _, err := e.pool.Exec(context.Background(), `UPDATE entitlements SET status = 'ended' WHERE account_id = $1`, e.acct); err != nil {
		t.Fatal(err)
	}
	e.reconcile(t)
	if h := e.holders(t); !slices.Equal(h, sorted(a, b)) {
		t.Fatalf("ended: holders = %v", h)
	}
	e.entitle(t, "suspended", 1)
	e.reconcile(t)
	if h := e.holders(t); !slices.Equal(h, sorted(b)) {
		t.Fatalf("suspended with lower limit: holders = %v, want b", h)
	}
	e.sharedIntact(t)
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
