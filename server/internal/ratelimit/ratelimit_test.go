// SPDX-License-Identifier: AGPL-3.0-or-later

package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/testkv"
)

func TestAllowCountReset(t *testing.T) {
	l := Limiter{KV: testkv.New(t)}
	ctx := context.Background()
	r := Rule{Name: "test", Limit: 3, Window: time.Minute}
	subject := uuid.NewString()
	for i := range 3 {
		ok, _, err := l.Allow(ctx, r, subject)
		if err != nil || !ok {
			t.Fatalf("attempt %d: ok=%v err=%v", i+1, ok, err)
		}
	}
	ok, retry, err := l.Allow(ctx, r, subject)
	if err != nil || ok || retry <= 0 || retry > time.Minute {
		t.Fatalf("4th attempt: ok=%v retry=%v err=%v", ok, retry, err)
	}
	n, ttl, err := l.Count(ctx, r, subject)
	if err != nil || n != 4 || ttl <= 0 {
		t.Fatalf("count=%d ttl=%v err=%v", n, ttl, err)
	}
	// 其他主体不受影响。
	if ok, _, _ := l.Allow(ctx, r, uuid.NewString()); !ok {
		t.Fatal("other subject limited")
	}
	if err := l.Reset(ctx, r, subject); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := l.Count(ctx, r, subject); n != 0 {
		t.Fatalf("count after reset = %d", n)
	}
}

func TestWindowExpires(t *testing.T) {
	l := Limiter{KV: testkv.New(t)}
	ctx := context.Background()
	r := Rule{Name: "short", Limit: 1, Window: 200 * time.Millisecond}
	subject := uuid.NewString()
	if ok, _, _ := l.Allow(ctx, r, subject); !ok {
		t.Fatal("first attempt limited")
	}
	if ok, _, _ := l.Allow(ctx, r, subject); ok {
		t.Fatal("second attempt allowed")
	}
	deadline := time.After(3 * time.Second)
	for {
		if ok, _, _ := l.Allow(ctx, r, subject); ok {
			return
		}
		select {
		case <-deadline:
			t.Fatal("window did not expire")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
