// SPDX-License-Identifier: AGPL-3.0-or-later

package db_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/akari-project/panel/server/internal/db"
	"github.com/akari-project/panel/server/internal/testdb"
)

const (
	sqlstateCheckViolation    = "23514"
	sqlstateRestrictViolation = "23001"
)

// 只追加表（CONV-18）。
var appendOnlyTables = []string{"entitlement_events", "credit_ledger", "audit_logs", "payment_notifications"}

// 分区明细表与纯关联表不带 updated_at（CONV-17）。
var noUpdatedAt = []string{
	"traffic_hourly", "traffic_hourly_default",
	"account_roles", "node_group_members", "plan_groups", "coupon_redemptions", "consumed_events",
	"goose_db_version",
}

func wantSQLState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("want SQLSTATE %s, got %v", code, err)
	}
	if pgErr.Code != code {
		t.Fatalf("want SQLSTATE %s, got %s: %s", code, pgErr.Code, pgErr.Message)
	}
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func mustID(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return id
}

func TestMigrateIsIdempotentAndVersionChecked(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	v, err := db.Migrate(ctx, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := db.RequiredVersion()
	if v != req {
		t.Fatalf("version %d, want %d", v, req)
	}
	cur, _, err := db.CheckVersion(ctx, pool)
	if err != nil || cur != req {
		t.Fatalf("CheckVersion = %d, %v", cur, err)
	}
	// 回退版本记录，模拟旧库：启动校验必须拒绝（DEP-12）。
	mustExec(t, pool, `DELETE FROM goose_db_version WHERE version_id = $1`, req)
	if _, _, err := db.CheckVersion(ctx, pool); !errors.Is(err, db.ErrSchemaTooOld) {
		t.Fatalf("CheckVersion on old schema = %v, want ErrSchemaTooOld", err)
	}
}

func TestCheckVersionOnEmptyDatabase(t *testing.T) {
	pool := testdb.New(t)
	mustExec(t, pool, `DROP TABLE goose_db_version`)
	if _, _, err := db.CheckVersion(context.Background(), pool); !errors.Is(err, db.ErrSchemaTooOld) {
		t.Fatalf("CheckVersion = %v, want ErrSchemaTooOld", err)
	}
}

// CONV-17：所有表有 created_at；可变表有 updated_at 与 touch_updated_at 触发器。
func TestSchemaConventions(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	rows, err := pool.Query(ctx, `
		SELECT c.relname,
		       EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid = c.oid AND a.attname = 'created_at' AND NOT a.attisdropped
		                 AND a.atttypid = 'timestamptz'::regtype AND a.attnotnull),
		       EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid = c.oid AND a.attname = 'updated_at' AND NOT a.attisdropped
		                 AND a.atttypid = 'timestamptz'::regtype AND a.attnotnull),
		       EXISTS (SELECT 1 FROM pg_trigger tg WHERE tg.tgrelid = c.oid AND tg.tgfoid = 'touch_updated_at'::regproc)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r','p') AND c.relname <> 'goose_db_version'
		ORDER BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		var created, updated, touch bool
		if err := rows.Scan(&name, &created, &updated, &touch); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
		if !created {
			t.Errorf("%s: missing created_at timestamptz NOT NULL", name)
		}
		exempt := slices.Contains(appendOnlyTables, name) || slices.Contains(noUpdatedAt, name)
		if exempt {
			continue
		}
		if !updated {
			t.Errorf("%s: mutable table without updated_at", name)
		}
		if !touch {
			t.Errorf("%s: mutable table without touch_updated_at trigger", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// spec/03 3.1 列出的表必须全部存在。
	for _, want := range []string{
		"accounts", "roles", "account_roles", "staff_invitations", "mfa_totp", "mfa_webauthn", "verification_codes", "sessions", "devices",
		"proxy_credentials", "export_tokens",
		"kernels", "kernel_protocols", "kernel_transports", "machines", "nodes", "location_groups", "node_group_members", "inbounds", "node_routes",
		"plans", "plan_groups", "plan_prices", "addon_prices", "entitlements", "entitlement_events", "addons", "usage_cycles",
		"quotes", "orders", "payment_providers", "payment_notifications", "refunds", "credit_ledger", "coupons", "coupon_redemptions",
		"redeem_codes", "redeem_redemptions",
		"ingest_batches", "traffic_hourly", "traffic_daily",
		"announcements", "support_tickets", "support_messages", "support_attachments", "articles", "referral_earnings",
		"notification_templates", "notification_preferences", "notification_outbox",
		"settings", "outbox", "consumed_events", "idempotency_keys", "job_fencing", "audit_logs",
	} {
		if !slices.Contains(tables, want) {
			t.Errorf("table %s missing (spec/03 3.1)", want)
		}
	}
}

func TestTouchUpdatedAt(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	mustExec(t, pool, `INSERT INTO settings (key, value, updated_at) VALUES ('k', '1', '2000-01-01T00:00:00Z')`)
	mustExec(t, pool, `UPDATE settings SET value = '2' WHERE key = 'k'`)
	var old bool
	if err := pool.QueryRow(ctx, `SELECT updated_at < '2001-01-01' FROM settings WHERE key = 'k'`).Scan(&old); err != nil {
		t.Fatal(err)
	}
	if old {
		t.Error("updated_at not refreshed by touch_updated_at")
	}
}

// fixture 建立只追加表测试所需的最少数据。
type fixture struct {
	account, plan, price, entitlement, order string
}

func newFixture(t *testing.T, pool *pgxpool.Pool) fixture {
	t.Helper()
	var f fixture
	f.account = mustID(t, pool, `INSERT INTO accounts (email, referral_code) VALUES ('a@example.com', 'REF1') RETURNING id`)
	f.plan = mustID(t, pool, `INSERT INTO plans (name, tier, bytes_per_cycle, device_limit) VALUES ('Basic', 1, 0, 3) RETURNING id`)
	f.price = mustID(t, pool, `INSERT INTO plan_prices (plan_id, period, amount_minor, currency) VALUES ($1, 'month', 3000, 'CNY') RETURNING id`, f.plan)
	f.entitlement = mustID(t, pool, `INSERT INTO entitlements (account_id, plan_id, locked_price_id, status, starts_at, expires_at, cycle_start,
		bytes_limit, device_limit, reset_policy) VALUES ($1, $2, $3, 'active', '2026-10-01Z', '2026-11-01Z', '2026-10-01Z', 0, 3, 'purchase_anchor') RETURNING id`,
		f.account, f.plan, f.price)
	quote := mustID(t, pool, `INSERT INTO quotes (account_id, order_type, price_id, breakdown, amount_due_minor, currency, expires_at)
		VALUES ($1, 'new', $2, '{}', 3000, 'CNY', '2026-10-01T00:15:00Z') RETURNING id`, f.account, f.price)
	f.order = mustID(t, pool, `INSERT INTO orders (number, account_id, type, quote_id, price_id, amount_due_minor, currency, expires_at)
		VALUES ('ORD-20261001-K7Q2XM', $1, 'new', $2, $3, 3000, 'CNY', '2026-10-01T00:30:00Z') RETURNING id`, f.account, quote, f.price)
	return f
}

// CONV-18：只追加表禁止 UPDATE、DELETE、TRUNCATE。
func TestAppendOnlyTables(t *testing.T) {
	pool := testdb.New(t)
	f := newFixture(t, pool)
	mustExec(t, pool, `INSERT INTO entitlement_events (entitlement_id, account_id, type, order_id, diff) VALUES ($1, $2, 'purchase', $3, '{}')`,
		f.entitlement, f.account, f.order)
	mustExec(t, pool, `INSERT INTO credit_ledger (account_id, amount_minor, currency, reason, order_id) VALUES ($1, 500, 'CNY', 'redeem', NULL)`, f.account)
	mustExec(t, pool, `INSERT INTO audit_logs (action, target_type, target_id, request_id, reason) VALUES ('plans.update', 'plan', $1, 'req_1', 'r')`, f.plan)
	mustExec(t, pool, `INSERT INTO payment_notifications (provider, order_id, is_verified, result, raw, received_at)
		VALUES ('alipay_f2f', $1, true, 'processed', '{}', '2026-10-01T00:01:00Z')`, f.order)

	ctx := context.Background()
	for _, table := range appendOnlyTables {
		t.Run(table, func(t *testing.T) {
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 1 {
				t.Fatalf("fixture row count = %d, %v", n, err)
			}
			_, err := pool.Exec(ctx, `UPDATE `+table+` SET created_at = created_at`)
			wantSQLState(t, err, sqlstateRestrictViolation)
			_, err = pool.Exec(ctx, `DELETE FROM `+table)
			wantSQLState(t, err, sqlstateRestrictViolation)
			_, err = pool.Exec(ctx, `TRUNCATE `+table+` CASCADE`)
			wantSQLState(t, err, sqlstateRestrictViolation)
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 1 {
				t.Fatalf("row count after rejected mutations = %d, %v", n, err)
			}
		})
	}
}

// ORD-16：余额流水的方向由原因决定。
func TestCreditLedgerDirection(t *testing.T) {
	pool := testdb.New(t)
	f := newFixture(t, pool)
	ctx := context.Background()
	for _, tc := range []struct {
		reason string
		amount int64
		ok     bool
	}{
		{"order_payment", -100, true},
		{"order_payment", 100, false},
		{"account_deletion", -100, true},
		{"refund", 100, true},
		{"refund", -100, false},
		{"admin_adjust", -100, true},
		{"admin_adjust", 100, true},
		{"referral", -100, true},
		{"proration_refund", 100, false},
	} {
		_, err := pool.Exec(ctx, `INSERT INTO credit_ledger (account_id, amount_minor, currency, reason) VALUES ($1, $2, 'CNY', $3)`,
			f.account, tc.amount, tc.reason)
		if tc.ok && err != nil {
			t.Errorf("%s %d: %v", tc.reason, tc.amount, err)
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("%s %d: accepted, want check violation", tc.reason, tc.amount)
			} else {
				wantSQLState(t, err, sqlstateCheckViolation)
			}
		}
	}
}

// BIL-01、spec/03 3.3：价格行只能停售；金额、币种、周期、period_days 不可修改；不可删除；非免费套餐金额大于 0。
func TestPlanPriceRows(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	plan := mustID(t, pool, `INSERT INTO plans (name, tier, kind, bytes_per_cycle, device_limit) VALUES ('Pass', 1, 'one_time', 0, 1) RETURNING id`)
	price := mustID(t, pool, `INSERT INTO plan_prices (plan_id, period, period_days, amount_minor, currency) VALUES ($1, 'one_time', NULL, 1000, 'CNY') RETURNING id`, plan)
	other := mustID(t, pool, `INSERT INTO plans (name, tier, bytes_per_cycle, device_limit) VALUES ('Other', 2, 0, 1) RETURNING id`)

	for name, stmt := range map[string]string{
		"amount":               `UPDATE plan_prices SET amount_minor = 999 WHERE id = $1`,
		"currency":             `UPDATE plan_prices SET currency = 'USD' WHERE id = $1`,
		"period":               `UPDATE plan_prices SET period = 'month' WHERE id = $1`,
		"period_days null→set": `UPDATE plan_prices SET period_days = 30 WHERE id = $1`,
		"plan_id":              `UPDATE plan_prices SET plan_id = '` + other + `' WHERE id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := pool.Exec(ctx, stmt, price)
			wantSQLState(t, err, sqlstateRestrictViolation)
		})
	}

	t.Run("period_days set→null", func(t *testing.T) {
		p := mustID(t, pool, `INSERT INTO plan_prices (plan_id, period, period_days, amount_minor, currency) VALUES ($1, 'month', 30, 500, 'CNY') RETURNING id`, other)
		_, err := pool.Exec(ctx, `UPDATE plan_prices SET period_days = NULL WHERE id = $1`, p)
		wantSQLState(t, err, sqlstateRestrictViolation)
	})

	t.Run("stop sale then cannot resume", func(t *testing.T) {
		mustExec(t, pool, `UPDATE plan_prices SET on_sale = false WHERE id = $1`, price)
		_, err := pool.Exec(ctx, `UPDATE plan_prices SET on_sale = true WHERE id = $1`, price)
		wantSQLState(t, err, sqlstateRestrictViolation)
		// 停售后可以新建同周期的在售行（改价的做法）。
		mustExec(t, pool, `INSERT INTO plan_prices (plan_id, period, amount_minor, currency) VALUES ($1, 'one_time', 1200, 'CNY')`, plan)
	})

	t.Run("only one on-sale row per period", func(t *testing.T) {
		_, err := pool.Exec(ctx, `INSERT INTO plan_prices (plan_id, period, amount_minor, currency) VALUES ($1, 'one_time', 1300, 'CNY')`, plan)
		wantSQLState(t, err, "23505")
	})

	t.Run("delete", func(t *testing.T) {
		_, err := pool.Exec(ctx, `DELETE FROM plan_prices WHERE id = $1`, price)
		wantSQLState(t, err, sqlstateRestrictViolation)
		_, err = pool.Exec(ctx, `TRUNCATE plan_prices CASCADE`)
		wantSQLState(t, err, sqlstateRestrictViolation)
	})

	t.Run("amount must be positive", func(t *testing.T) {
		_, err := pool.Exec(ctx, `INSERT INTO plan_prices (plan_id, period, amount_minor, currency) VALUES ($1, 'year', 0, 'CNY')`, other)
		wantSQLState(t, err, sqlstateCheckViolation)
	})

	t.Run("free plan has no price rows", func(t *testing.T) {
		free := mustID(t, pool, `INSERT INTO plans (name, tier, kind, bytes_per_cycle, device_limit) VALUES ('Free', 0, 'free', 1024, 1) RETURNING id`)
		_, err := pool.Exec(ctx, `INSERT INTO plan_prices (plan_id, period, amount_minor, currency) VALUES ($1, 'month', 100, 'CNY')`, free)
		wantSQLState(t, err, sqlstateCheckViolation)
		_, err = pool.Exec(ctx, `UPDATE plans SET kind = 'free', tier = 0 WHERE id = $1`, plan)
		wantSQLState(t, err, sqlstateCheckViolation)
	})

	t.Run("tier 0 is reserved for the free plan", func(t *testing.T) {
		_, err := pool.Exec(ctx, `INSERT INTO plans (name, tier, bytes_per_cycle, device_limit) VALUES ('Zero', 0, 0, 1)`)
		wantSQLState(t, err, sqlstateCheckViolation)
	})
}

// BIL-24：加购项价格与套餐价格同样只增不改。
func TestAddonPriceRows(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	id := mustID(t, pool, `INSERT INTO addon_prices (kind, bytes_total, amount_minor, currency) VALUES ('traffic', 107374182400, 1000, 'CNY') RETURNING id`)
	for _, stmt := range []string{
		`UPDATE addon_prices SET amount_minor = 1 WHERE id = $1`,
		`UPDATE addon_prices SET currency = 'USD' WHERE id = $1`,
		`UPDATE addon_prices SET bytes_total = 1 WHERE id = $1`,
		`DELETE FROM addon_prices WHERE id = $1`,
	} {
		_, err := pool.Exec(ctx, stmt, id)
		wantSQLState(t, err, sqlstateRestrictViolation)
	}
	mustExec(t, pool, `UPDATE addon_prices SET on_sale = false WHERE id = $1`, id)
	_, err := pool.Exec(ctx, `UPDATE addon_prices SET on_sale = true WHERE id = $1`, id)
	wantSQLState(t, err, sqlstateRestrictViolation)
	_, err = pool.Exec(ctx, `INSERT INTO addon_prices (kind, device_slots, amount_minor, currency) VALUES ('devices', 1, 0, 'CNY')`)
	wantSQLState(t, err, sqlstateCheckViolation)
}

func newNode(t *testing.T, pool *pgxpool.Pool, kernel string, allowExperimental bool) string {
	t.Helper()
	return mustID(t, pool, `INSERT INTO nodes (name, region_code, public_host, kernel_type, allow_experimental)
		VALUES ('n', 'JP', 'n.example.com', $1, $2) RETURNING id`, kernel, allowExperimental)
}

func insertInbound(pool *pgxpool.Pool, node, protocol, transport string, port int) error {
	_, err := pool.Exec(context.Background(), `INSERT INTO inbounds (node_id, protocol, listen_port, settings) VALUES ($1, $2, $3, jsonb_build_object('transport', $4::text))`,
		node, protocol, port, transport)
	return err
}

// AGT-09、AGT-10、spec/03 3.3：入站协议与传输必须被节点内核支持，实验项需要节点允许。
func TestInboundKernelGuard(t *testing.T) {
	pool := testdb.New(t)
	singbox := newNode(t, pool, "singbox", false)
	xray := newNode(t, pool, "xray", false)
	xrayExp := newNode(t, pool, "xray", true)

	for i, tc := range []struct {
		name, node, protocol, transport string
		ok                              bool
	}{
		{"singbox vless tcp", singbox, "vless", "tcp", true},
		{"singbox tuic quic", singbox, "tuic", "quic", true},
		{"singbox anytls tcp", singbox, "anytls", "tcp", true},
		{"singbox vless xhttp unsupported transport", singbox, "vless", "xhttp", false},
		{"singbox vmess mkcp unsupported transport", singbox, "vmess", "mkcp", false},
		{"xray vless xhttp", xray, "vless", "xhttp", true},
		{"xray vmess mkcp", xray, "vmess", "mkcp", true},
		{"xray tuic unsupported protocol", xray, "tuic", "quic", false},
		{"xray anytls unsupported protocol", xray, "anytls", "tcp", false},
		{"xray hysteria2 experimental not allowed", xray, "hysteria2", "quic", false},
		{"xray hysteria2 experimental allowed", xrayExp, "hysteria2", "quic", true},
		{"unknown transport", singbox, "vless", "carrier-pigeon", false},
	} {
		port := 10000 + i
		t.Run(tc.name, func(t *testing.T) {
			err := insertInbound(pool, tc.node, tc.protocol, tc.transport, port)
			if tc.ok && err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if !tc.ok {
				wantSQLState(t, err, sqlstateCheckViolation)
			}
		})
	}

	t.Run("missing transport", func(t *testing.T) {
		_, err := pool.Exec(context.Background(), `INSERT INTO inbounds (node_id, protocol, listen_port, settings) VALUES ($1, 'vless', 20001, '{}')`, singbox)
		wantSQLState(t, err, sqlstateCheckViolation)
	})

	t.Run("disabled inbound is not checked, enabling it is", func(t *testing.T) {
		ctx := context.Background()
		id := mustID(t, pool, `INSERT INTO inbounds (node_id, protocol, listen_port, settings, enabled)
			VALUES ($1, 'tuic', 20002, '{"transport":"quic"}', false) RETURNING id`, xray)
		_, err := pool.Exec(ctx, `UPDATE inbounds SET enabled = true WHERE id = $1`, id)
		wantSQLState(t, err, sqlstateCheckViolation)
	})

	t.Run("changing transport is checked", func(t *testing.T) {
		ctx := context.Background()
		id := mustID(t, pool, `INSERT INTO inbounds (node_id, protocol, listen_port, settings) VALUES ($1, 'vless', 20003, '{"transport":"ws"}') RETURNING id`, singbox)
		_, err := pool.Exec(ctx, `UPDATE inbounds SET settings = '{"transport":"xhttp"}' WHERE id = $1`, id)
		wantSQLState(t, err, sqlstateCheckViolation)
	})
}

// spec/03 3.3 nodes_kernel_guard：切换内核或关闭实验开关时，已启用的入站必须仍被支持。
func TestNodeKernelGuard(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	t.Run("switch singbox→xray with tuic inbound", func(t *testing.T) {
		n := newNode(t, pool, "singbox", false)
		if err := insertInbound(pool, n, "tuic", "quic", 443); err != nil {
			t.Fatal(err)
		}
		_, err := pool.Exec(ctx, `UPDATE nodes SET kernel_type = 'xray' WHERE id = $1`, n)
		wantSQLState(t, err, sqlstateCheckViolation)
		// 停用不兼容的入站后可以切换。
		mustExec(t, pool, `UPDATE inbounds SET enabled = false WHERE node_id = $1`, n)
		mustExec(t, pool, `UPDATE nodes SET kernel_type = 'xray' WHERE id = $1`, n)
	})

	t.Run("switch xray→singbox with xhttp inbound", func(t *testing.T) {
		n := newNode(t, pool, "xray", false)
		if err := insertInbound(pool, n, "vless", "xhttp", 443); err != nil {
			t.Fatal(err)
		}
		_, err := pool.Exec(ctx, `UPDATE nodes SET kernel_type = 'singbox' WHERE id = $1`, n)
		wantSQLState(t, err, sqlstateCheckViolation)
	})

	t.Run("disable experimental with hysteria2 on xray", func(t *testing.T) {
		n := newNode(t, pool, "xray", true)
		if err := insertInbound(pool, n, "hysteria2", "quic", 443); err != nil {
			t.Fatal(err)
		}
		_, err := pool.Exec(ctx, `UPDATE nodes SET allow_experimental = false WHERE id = $1`, n)
		wantSQLState(t, err, sqlstateCheckViolation)
		// 切到 sing-box（hysteria2 稳定）可以。
		mustExec(t, pool, `UPDATE nodes SET kernel_type = 'singbox', allow_experimental = false WHERE id = $1`, n)
	})

	t.Run("compatible switch", func(t *testing.T) {
		n := newNode(t, pool, "singbox", false)
		if err := insertInbound(pool, n, "vless", "grpc", 443); err != nil {
			t.Fatal(err)
		}
		mustExec(t, pool, `UPDATE nodes SET kernel_type = 'xray' WHERE id = $1`, n)
	})
}

// CONV-26：站点时区初始化后只读。
func TestSiteTimezoneReadOnly(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	mustExec(t, pool, `INSERT INTO settings (key, value) VALUES ('site_timezone', '"Asia/Shanghai"')`)
	_, err := pool.Exec(ctx, `UPDATE settings SET value = '"UTC"' WHERE key = 'site_timezone'`)
	wantSQLState(t, err, sqlstateRestrictViolation)
	_, err = pool.Exec(ctx, `DELETE FROM settings WHERE key = 'site_timezone'`)
	wantSQLState(t, err, sqlstateRestrictViolation)
	mustExec(t, pool, `INSERT INTO settings (key, value) VALUES ('free_device_limit', '1')`)
	mustExec(t, pool, `UPDATE settings SET value = '2' WHERE key = 'free_device_limit'`)
}

// spec/03 3.3 的部分唯一索引。
func TestPartialUniqueIndexes(t *testing.T) {
	pool := testdb.New(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `INSERT INTO entitlements (account_id, plan_id, status, starts_at, cycle_start, bytes_limit, device_limit, reset_policy)
		VALUES ($1, $2, 'over_quota', '2026-10-01Z', '2026-10-01Z', 0, 1, 'never')`, f.account, f.plan)
	wantSQLState(t, err, "23505")

	quote := mustID(t, pool, `INSERT INTO quotes (account_id, order_type, price_id, breakdown, amount_due_minor, currency, expires_at)
		VALUES ($1, 'new', $2, '{}', 3000, 'CNY', '2026-10-01T00:15:00Z') RETURNING id`, f.account, f.price)
	_, err = pool.Exec(ctx, `INSERT INTO orders (number, account_id, type, quote_id, price_id, amount_due_minor, currency, expires_at)
		VALUES ('ORD-20261001-AAAAAA', $1, 'new', $2, $3, 3000, 'CNY', '2026-10-01T00:30:00Z')`, f.account, quote, f.price)
	wantSQLState(t, err, "23505") // orders_one_pending_new

	mustExec(t, pool, `INSERT INTO idempotency_keys (key, account_id, route, request_hash, expires_at)
		VALUES ('0192f0c4-ffa0-7c3d-8000-000000000001', NULL, 'POST /v1/accounts', '\x00', '2026-10-02Z')`)
	_, err = pool.Exec(ctx, `INSERT INTO idempotency_keys (key, account_id, route, request_hash, expires_at)
		VALUES ('0192f0c4-ffa0-7c3d-8000-000000000001', NULL, 'POST /v1/accounts', '\x00', '2026-10-02Z')`)
	wantSQLState(t, err, "23505")
	// 同一个键在另一个路由、或属于某个账号时，不冲突。
	mustExec(t, pool, `INSERT INTO idempotency_keys (key, account_id, route, request_hash, expires_at)
		VALUES ('0192f0c4-ffa0-7c3d-8000-000000000001', NULL, 'POST /v1/password-resets', '\x00', '2026-10-02Z')`)
	mustExec(t, pool, `INSERT INTO idempotency_keys (key, account_id, route, request_hash, expires_at)
		VALUES ('0192f0c4-ffa0-7c3d-8000-000000000001', $1, 'POST /v1/orders', '\x00', '2026-10-02Z')`, f.account)

	_, err = pool.Exec(ctx, `INSERT INTO coupons (code_hash, kind, value) VALUES ('h', 'percent', 10001)`)
	wantSQLState(t, err, sqlstateCheckViolation)
}

// 内置角色的权限只来自 AUTH-17 的权限目录。
func TestBuiltinRoles(t *testing.T) {
	pool := testdb.New(t)
	catalog := []string{"*", "accounts.read", "accounts.adjust", "credits.adjust", "orders.read", "orders.refund", "plans.*",
		"location-groups.*", "hosts.*", "kernels.write", "coupons.*", "content.*", "tickets.*", "payments.configure",
		"settings.read", "settings.write", "staff.*", "audit.read"}
	rows, err := pool.Query(context.Background(), `SELECT name, permissions FROM roles WHERE is_builtin ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
		var name string
		var perms []string
		if err := r.Scan(&name, &perms); err != nil {
			return "", err
		}
		for _, p := range perms {
			if !slices.Contains(catalog, p) {
				t.Errorf("role %s: permission %q not in AUTH-17 catalog", name, p)
			}
		}
		return name, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"operator", "superadmin", "support"}) {
		t.Errorf("builtin roles = %v", got)
	}
}
