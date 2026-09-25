// SPDX-License-Identifier: AGPL-3.0-or-later

package consoleapi

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/consoleapi/gen"
	"github.com/akari-project/panel/server/internal/db/sqlc"
)

type auditRow struct {
	ID         uuid.UUID
	ActorID    *uuid.UUID
	ActorEmail *string
	Action     string
	TargetType string
	TargetID   *string
	Diff       []byte
	Reason     *string
	IpPrefix   *string
	RequestID  *string
	CreatedAt  time.Time
}

func nullOf[T any](p *T) nullable.Nullable[T] {
	if p == nil {
		return nullable.NewNullNullable[T]()
	}
	return nullable.NewNullableWithValue(*p)
}

func (r auditRow) api() (gen.AuditLog, error) {
	out := gen.AuditLog{
		Id: r.ID, ActorId: nullOf(r.ActorID), ActorEmail: nullOf(r.ActorEmail), Action: r.Action, TargetType: r.TargetType,
		TargetId: nullOf(r.TargetID), IpPrefix: nullOf(r.IpPrefix), CreatedAt: r.CreatedAt,
		Diff: nullable.NewNullNullable[map[string]any](), Reason: nullable.NewNullNullable[string](),
	}
	if r.RequestID != nil {
		out.RequestId = *r.RequestID
	}
	// 账号个人数据删除后原因原文为空串（CONV-29），按没有原因显示。
	if r.Reason != nil && *r.Reason != "" {
		out.Reason = nullable.NewNullableWithValue(*r.Reason)
	}
	if len(r.Diff) > 0 {
		var d map[string]any
		if err := json.Unmarshal(r.Diff, &d); err != nil {
			return gen.AuditLog{}, err
		}
		out.Diff = nullable.NewNullableWithValue(d)
	}
	return out, nil
}

// ListAuditLogs 查询审计日志（只读，AUTH-18），按时间倒序游标分页（CONV-11）；created_to 含端点。
func (s *Server) ListAuditLogs(ctx context.Context, req gen.ListAuditLogsRequestObject) (gen.ListAuditLogsResponseObject, error) {
	limit, err := pageLimit(req.Params.Limit)
	if err != nil {
		return nil, err
	}
	var cur struct {
		At time.Time `json:"t"`
		ID uuid.UUID `json:"id"`
	}
	p := sqlc.ListAuditLogsParams{
		ActorID: req.Params.ActorId, Action: req.Params.Action, TargetType: req.Params.TargetType, TargetID: req.Params.TargetId,
		CreatedFrom: req.Params.CreatedFrom, CreatedTo: req.Params.CreatedTo, MaxRows: limit + 1,
	}
	if ok, err := decodeCursor(req.Params.Cursor, &cur); err != nil {
		return nil, err
	} else if ok {
		p.CursorAt, p.CursorID = &cur.At, &cur.ID
	}
	rows, err := sqlc.New(s.d.Pool).ListAuditLogs(ctx, p)
	if err != nil {
		return nil, err
	}
	var next *string
	if len(rows) > int(limit) {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		next = encodeCursor(map[string]any{"t": last.CreatedAt, "id": last.ID})
	}
	resp := gen.ListAuditLogs200JSONResponse{Items: make([]gen.AuditLog, len(rows)), NextCursor: nullableString(next)}
	for i, r := range rows {
		if resp.Items[i], err = auditRow(r).api(); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// GetAuditLog 返回一条审计日志（只读）。
func (s *Server) GetAuditLog(ctx context.Context, req gen.GetAuditLogRequestObject) (gen.GetAuditLogResponseObject, error) {
	r, err := sqlc.New(s.d.Pool).GetAuditLog(ctx, req.Id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apierr.NotFound
	}
	if err != nil {
		return nil, err
	}
	out, err := auditRow(r).api()
	if err != nil {
		return nil, err
	}
	return gen.GetAuditLog200JSONResponse(out), nil
}
