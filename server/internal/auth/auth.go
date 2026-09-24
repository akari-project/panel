// SPDX-License-Identifier: AGPL-3.0-or-later

// Package auth 是认证的公共部分：请求的认证主体，以及会话吊销集合（spec/10 AUTH-06）。
package auth

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/valkey-io/valkey-go"

	"github.com/akari-project/panel/server/internal/auth/token"
)

// Principal 是已认证请求的主体。
type Principal struct {
	AccountID uuid.UUID
	SessionID uuid.UUID
	Audience  token.Audience
	AMR       []string
}

type ctxKey struct{}

// WithPrincipal 把主体放入 ctx。
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext 返回 ctx 中的主体；未认证时 ok 为 false。
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// Revocations 是会话吊销集合：会话被吊销时写入 sid，TTL 为访问令牌有效期，
// 之后该会话签发过的访问令牌都已过期，无需再保留（AUTH-06）。
type Revocations struct {
	KV valkey.Client
}

func revokedKey(sid uuid.UUID) string { return "auth:revoked:" + sid.String() }

// Timeout 是单次 Valkey 调用的时限：Valkey 卡住时尽快返回 503，而不是让请求挂起（spec/40 40.4）。
const Timeout = 2 * time.Second

// Revoke 把会话加入吊销集合。
func (r Revocations) Revoke(ctx context.Context, sids ...uuid.UUID) error {
	if len(sids) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	cmds := make(valkey.Commands, 0, len(sids))
	for _, sid := range sids {
		cmds = append(cmds, r.KV.B().Set().Key(revokedKey(sid)).Value("1").Ex(token.TTL).Build())
	}
	for _, res := range r.KV.DoMulti(ctx, cmds...) {
		if err := res.Error(); err != nil {
			return err
		}
	}
	return nil
}

// IsRevoked 报告会话是否在吊销集合中。Valkey 不可用时返回错误，调用方拒绝请求而不是放行。
func (r Revocations) IsRevoked(ctx context.Context, sid uuid.UUID) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	n, err := r.KV.Do(ctx, r.KV.B().Exists().Key(revokedKey(sid)).Build()).AsInt64()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
