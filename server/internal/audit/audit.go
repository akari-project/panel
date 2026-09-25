// SPDX-License-Identifier: AGPL-3.0-or-later

// Package audit 写入审计日志（spec/10 AUTH-18，spec/31 CON-03、CON-09）：
//
//   - 在调用方的事务中写入，与业务变更同时提交或回滚；
//   - 操作者取自请求的认证主体，来源 IP 只记录 /24 或 /48 前缀（CONV-24），request_id 取自请求；
//     没有请求时（命令行）生成 UUIDv7 作为 request_id，操作者为空；
//   - 原因原文写入可变表 reason_texts，audit_logs 只保存 reason_id（CONV-29）；
//   - 差异中以 _enc 或 _hash 结尾的键（包括 settings 中以 _enc 结尾的键）只记录“已修改”，不记录取值。
//
// 差异（diff）的形状：创建与删除记录对象的取值 {字段: 值}；修改只记录变化的字段 {字段: {"from": 旧值, "to": 新值}}。
// 被隐藏的取值写作 {"changed": true}。
package audit

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/auth"
	"github.com/akari-project/panel/server/internal/db/sqlc"
	"github.com/akari-project/panel/server/internal/httpx"
)

// Entry 是一条审计记录。Action 与 TargetType 的取值见 spec/31 CON-09。
type Entry struct {
	Action     string
	TargetType string
	// TargetID 为空时不记录。
	TargetID string
	// Diff 为差异，由 Values 或 Changes 构造；nil 表示没有差异。
	Diff map[string]any
	// Reason 是操作原因原文；为空时不记录。
	Reason string
	// ReasonAccount 是原因所涉及的账号：删除该账号个人数据时清空原因原文（CONV-29）。
	ReasonAccount *uuid.UUID
	// Actor 覆盖操作者；为 nil 时取请求的认证主体（未认证请求为空）。
	Actor *uuid.UUID
}

type sourceKey struct{}

// WithIPPrefix 记录请求来源的 IP 前缀（/24 或 /48，CONV-24），由接口的路由在处理前放入。
func WithIPPrefix(ctx context.Context, prefix string) context.Context {
	return context.WithValue(ctx, sourceKey{}, prefix)
}

// Record 在调用方的事务中写入一条审计记录。
func Record(ctx context.Context, q *sqlc.Queries, e Entry) error {
	actor := e.Actor
	if actor == nil {
		if p, ok := auth.FromContext(ctx); ok {
			id := p.AccountID
			actor = &id
		}
	}
	var reasonID *uuid.UUID
	if e.Reason != "" {
		id, err := q.InsertReasonText(ctx, sqlc.InsertReasonTextParams{AccountID: e.ReasonAccount, Body: e.Reason})
		if err != nil {
			return err
		}
		reasonID = &id
	}
	var diff []byte
	if e.Diff != nil {
		var err error
		if diff, err = redactJSON(e.Diff); err != nil {
			return err
		}
	}
	rid := httpx.RequestID(ctx)
	if rid == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		rid = id.String()
	}
	prefix, _ := ctx.Value(sourceKey{}).(string)
	return q.InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
		ActorID:    actor,
		Action:     e.Action,
		TargetType: e.TargetType,
		TargetID:   nilIfEmpty(e.TargetID),
		Diff:       diff,
		IpPrefix:   nilIfEmpty(prefix),
		RequestID:  &rid,
		ReasonID:   reasonID,
	})
}

// Values 是创建或删除时记录的取值 {字段: 值}。
func Values(kv map[string]any) map[string]any { return kv }

// Changes 返回 before 与 after 中取值不同的字段 {字段: {"from", "to"}}（按 JSON 形式比较）。
// 只在一侧出现的字段，另一侧记为 null。没有变化时返回空映射。
func Changes(before, after map[string]any) map[string]any {
	out := map[string]any{}
	for k, b := range before {
		a, ok := after[k]
		if !ok || !sameJSON(a, b) {
			out[k] = map[string]any{"from": b, "to": a}
		}
	}
	for k, a := range after {
		if _, ok := before[k]; !ok {
			out[k] = map[string]any{"from": nil, "to": a}
		}
	}
	return out
}

func sameJSON(a, b any) bool {
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return reflect.DeepEqual(a, b)
	}
	var va, vb any
	_ = json.Unmarshal(ja, &va)
	_ = json.Unmarshal(jb, &vb)
	return reflect.DeepEqual(va, vb)
}

// Hidden 是被隐藏的取值（AUTH-18）。
var Hidden = map[string]any{"changed": true}

// Secret 报告字段的取值是否只能记录为“已修改”：以 _enc 或 _hash 结尾（AUTH-18、CONV-19、CONV-20）。
func Secret(key string) bool {
	return strings.HasSuffix(key, "_enc") || strings.HasSuffix(key, "_hash")
}

// redactJSON 先把差异转为通用的 JSON 值（使嵌套的结构体与类型化映射同样被检查），再隐藏敏感键的取值。
func redactJSON(diff map[string]any) ([]byte, error) {
	raw, err := json.Marshal(diff)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	return json.Marshal(redact(generic))
}

// redact 递归隐藏敏感键的取值：键本身保留，取值改为 {"changed": true}。
func redact(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			if Secret(k) {
				out[k] = Hidden
				continue
			}
			out[k] = redact(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = redact(x)
		}
		return out
	}
	return v
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
