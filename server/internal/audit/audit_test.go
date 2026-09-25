// SPDX-License-Identifier: AGPL-3.0-or-later

package audit

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/auth"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/testdb"
)

func TestChanges(t *testing.T) {
	got := Changes(
		map[string]any{"roles": []string{"operator"}, "description": "a", "same": 1},
		map[string]any{"roles": []any{"support"}, "same": 1.0, "added": true},
	)
	want := `{"added":{"from":null,"to":true},"description":{"from":"a","to":null},"roles":{"from":["operator"],"to":["support"]}}`
	if b, _ := json.Marshal(got); string(b) != want {
		t.Fatalf("Changes = %s", b)
	}
	if len(Changes(map[string]any{"a": []string{"x"}}, map[string]any{"a": []any{"x"}})) != 0 {
		t.Fatal("equal JSON values reported as changed")
	}
}

// _enc 与 _hash 结尾的键只记录“已修改”，嵌套的值与类型化映射同样处理（AUTH-18）。
func TestRedact(t *testing.T) {
	type provider struct {
		AppID         string `json:"app_id"`
		PrivateKeyEnc string `json:"private_key_enc"`
	}
	b, err := redactJSON(map[string]any{
		"smtp_password_enc": Changes(map[string]any{"v": "old"}, map[string]any{"v": "new"}),
		"token_hash":        "abc",
		"alipay":            provider{AppID: "2021", PrivateKeyEnc: "secret"},
		"items":             []map[string]string{{"code_hash": "h", "name": "n"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"alipay":{"app_id":"2021","private_key_enc":{"changed":true}},"items":[{"code_hash":{"changed":true},"name":"n"}],"smtp_password_enc":{"changed":true},"token_hash":{"changed":true}}`
	if string(b) != want {
		t.Fatalf("redacted = %s", b)
	}
}

func TestRecord(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	q := sqlc.New(pool)
	var actor, target uuid.UUID
	for _, id := range []*uuid.UUID{&actor, &target} {
		if err := pool.QueryRow(ctx, `INSERT INTO accounts (email, referral_code) VALUES ($1, $2) RETURNING id`,
			uuid.NewString()+"@example.com", uuid.NewString()[:8]).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	rctx := WithIPPrefix(auth.WithPrincipal(ctx, auth.Principal{AccountID: actor}), "198.51.100.0/24")
	if err := Record(rctx, q, Entry{Action: "staff.update", TargetType: "account", TargetID: target.String(),
		Diff:   Changes(map[string]any{"roles": []string{"operator"}}, map[string]any{"roles": []string{"support"}}),
		Reason: "离职交接", ReasonAccount: &target}); err != nil {
		t.Fatal(err)
	}
	var (
		gotActor  *uuid.UUID
		prefix    *string
		requestID *string
		reason    string
		reasonAcc *uuid.UUID
	)
	if err := pool.QueryRow(ctx, `SELECT l.actor_id, l.ip_prefix, l.request_id, r.body, r.account_id
		FROM audit_logs l JOIN reason_texts r ON r.id = l.reason_id`).Scan(&gotActor, &prefix, &requestID, &reason, &reasonAcc); err != nil {
		t.Fatal(err)
	}
	if gotActor == nil || *gotActor != actor || prefix == nil || *prefix != "198.51.100.0/24" || reason != "离职交接" ||
		reasonAcc == nil || *reasonAcc != target {
		t.Fatalf("actor %v prefix %v reason %q account %v", gotActor, prefix, reason, reasonAcc)
	}
	// 没有请求（命令行）时生成 UUIDv7 作为 request_id，操作者为空（AUTH-18）。
	if err := Record(ctx, q, Entry{Action: "staff.create", TargetType: "account", TargetID: target.String()}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT actor_id, request_id FROM audit_logs WHERE action = 'staff.create'`).Scan(&gotActor, &requestID); err != nil {
		t.Fatal(err)
	}
	if gotActor != nil || requestID == nil {
		t.Fatalf("actor %v request_id %v", gotActor, requestID)
	}
	if id, err := uuid.Parse(*requestID); err != nil || id.Version() != 7 {
		t.Fatalf("request_id %q is not a UUIDv7", *requestID)
	}
}
