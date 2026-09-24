// SPDX-License-Identifier: AGPL-3.0-or-later

package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/clock"
	"github.com/akari-project/panel/server/internal/password"
	"github.com/akari-project/panel/server/internal/secretbox"
	"github.com/akari-project/panel/server/internal/testdb"
)

func testKeys(t *testing.T) *secretbox.Keyring {
	t.Helper()
	k, err := secretbox.ParseKeyring("1:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestCreateFirstSuperadmin(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	keys := testKeys(t)
	c := &Creator{Pool: pool, Clock: clock.NewFake(now), Keys: keys, Password: password.Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1}}

	res, err := c.Create(ctx, "Admin@Example.com", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}

	var email, hash, code string
	var verified time.Time
	if err := pool.QueryRow(ctx, `SELECT email, password_hash, email_verified_at, referral_code FROM accounts WHERE id = $1`, res.AccountID).
		Scan(&email, &hash, &verified, &code); err != nil {
		t.Fatal(err)
	}
	if email != "admin@example.com" || !verified.Equal(now) || len(code) != 8 {
		t.Errorf("account = %s %v %s", email, verified, code)
	}
	if ok, _ := password.Verify("correct horse battery", hash); !ok {
		t.Error("password hash does not verify")
	}

	var role string
	if err := pool.QueryRow(ctx, `SELECT role FROM account_roles WHERE account_id = $1`, res.AccountID).Scan(&role); err != nil || role != SuperadminRole {
		t.Errorf("role = %q, %v", role, err)
	}

	var enc []byte
	if err := pool.QueryRow(ctx, `SELECT secret_enc FROM proxy_credentials WHERE id = $1 AND account_id = $2 AND device_id IS NULL`, res.CredentialID, res.AccountID).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	secret, err := keys.Open(enc, []byte("proxy_credentials.secret_enc"))
	if err != nil {
		t.Fatal(err)
	}
	// spec/21 AGT-15：凭据明文为 16 字节原始 UUIDv4。
	if u, err := uuid.FromBytes(secret); err != nil || u.Version() != 4 {
		t.Errorf("credential secret is not 16 raw bytes of a UUIDv4: %x", secret)
	}

	var topic string
	var payload []byte
	if err := pool.QueryRow(ctx, `SELECT topic, payload FROM outbox`).Scan(&topic, &payload); err != nil || topic != "credential.changed" {
		t.Errorf("outbox = %q, %v", topic, err)
	}
	var diff, reason string
	if err := pool.QueryRow(ctx, `SELECT a.diff::text, r.body FROM audit_logs a JOIN reason_texts r ON r.id = a.reason_id
		WHERE a.target_id = $1 AND r.account_id = $2`, res.AccountID.String(), res.AccountID).Scan(&diff, &reason); err != nil {
		t.Fatal(err)
	}
	if reason != "panel admin create" {
		t.Errorf("reason = %q", reason)
	}
	if strings.Contains(diff+string(payload), "example.com") {
		t.Error("audit log or outbox contains the email address (CONV-29)")
	}

	if _, err := c.Create(ctx, "second@example.com", "correct horse battery"); !errors.Is(err, ErrSuperadminExists) {
		t.Errorf("second create = %v, want ErrSuperadminExists", err)
	}
}

func TestCreateValidation(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	c := &Creator{Pool: pool, Clock: clock.NewFake(time.Unix(0, 0)), Keys: testKeys(t), Password: password.Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1}}
	if _, err := c.Create(ctx, "not-an-email", "correct horse battery"); !errors.Is(err, ErrInvalidEmail) {
		t.Errorf("bad email = %v", err)
	}
	if _, err := c.Create(ctx, "Name <a@example.com>", "correct horse battery"); !errors.Is(err, ErrInvalidEmail) {
		t.Errorf("display-name email = %v", err)
	}
	if _, err := c.Create(ctx, "a@example.com", "short"); !errors.Is(err, password.ErrLength) {
		t.Errorf("short password = %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO accounts (email, referral_code) VALUES ('taken@example.com', 'X')`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Create(ctx, "TAKEN@example.com", "correct horse battery"); !errors.Is(err, ErrEmailTaken) {
		t.Errorf("taken email = %v", err)
	}
}
