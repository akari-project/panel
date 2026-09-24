// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/testkv"
)

func TestRevocations(t *testing.T) {
	r := Revocations{KV: testkv.New(t)}
	ctx := context.Background()
	a, b := uuid.New(), uuid.New()
	if revoked, err := r.IsRevoked(ctx, a); err != nil || revoked {
		t.Fatalf("revoked=%v err=%v", revoked, err)
	}
	if err := r.Revoke(ctx, a); err != nil {
		t.Fatal(err)
	}
	if revoked, _ := r.IsRevoked(ctx, a); !revoked {
		t.Fatal("not revoked")
	}
	if revoked, _ := r.IsRevoked(ctx, b); revoked {
		t.Fatal("unrelated session revoked")
	}
	ttl, err := r.KV.Do(ctx, r.KV.B().Ttl().Key(revokedKey(a)).Build()).AsInt64()
	if err != nil || ttl <= 0 || ttl > 15*60 {
		t.Fatalf("ttl=%d err=%v, want within 15 minutes", ttl, err)
	}
}

func TestPrincipalContext(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("empty context has principal")
	}
	p := Principal{AccountID: uuid.New()}
	got, ok := FromContext(WithPrincipal(context.Background(), p))
	if !ok || got.AccountID != p.AccountID {
		t.Fatal("principal lost")
	}
}
