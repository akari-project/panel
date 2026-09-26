// SPDX-License-Identifier: AGPL-3.0-or-later

package clientapi

import (
	"context"

	"github.com/oapi-codegen/nullable"

	"github.com/akari-project/panel/server/internal/auth"
	"github.com/akari-project/panel/server/internal/clientapi/gen"
	"github.com/akari-project/panel/server/internal/db/sqlc"
)

// ListDevices 返回未吊销的设备（含 web 设备），不分页（CONV-11）；device_limit 与 GET /v1/me 口径相同（AUTH-14）。
func (s *Server) ListDevices(ctx context.Context, _ gen.ListDevicesRequestObject) (gen.ListDevicesResponseObject, error) {
	p, _ := auth.FromContext(ctx)
	q := sqlc.New(s.d.Pool)
	rows, err := q.ListDevices(ctx, sqlc.ListDevicesParams{AccountID: p.AccountID, SessionID: p.SessionID})
	if err != nil {
		return nil, err
	}
	limit, err := q.DeviceLimit(ctx, p.AccountID)
	if err != nil {
		return nil, err
	}
	out := gen.ListDevices200JSONResponse{DeviceLimit: int(limit), Items: make([]gen.Device, 0, len(rows))}
	for _, r := range rows {
		out.Items = append(out.Items, gen.Device{
			Id:            r.ID,
			Platform:      gen.Platform(r.Platform),
			Model:         nullableOf(r.Model),
			AppVersion:    nullableOf(r.AppVersion),
			CreatedAt:     r.CreatedAt,
			LastSeenAt:    nullableOf(r.LastSeenAt),
			IpPrefix:      nullableOf(r.IpPrefix),
			IsCurrent:     r.IsCurrent,
			HasCredential: r.HasCredential,
		})
	}
	return out, nil
}

// nullableOf 把可空列转为契约中的 [T, 'null'] 字段：两种情况都写出该字段。
func nullableOf[T any](v *T) nullable.Nullable[T] {
	if v == nil {
		return nullable.NewNullNullable[T]()
	}
	return nullable.NewNullableWithValue(*v)
}

// RemoveDevice 移除设备（AUTH-15）：吊销该设备的会话与代理凭据，等待中的设备随即取得名额（AUTH-14）。
// 移除当前设备等同于登出，同时清除 Cookie。不要求重新验证。
func (s *Server) RemoveDevice(ctx context.Context, req gen.RemoveDeviceRequestObject) (gen.RemoveDeviceResponseObject, error) {
	p, _ := auth.FromContext(ctx)
	current, err := s.d.Sessions.RemoveDevice(ctx, p, req.Id)
	if current {
		clearTokenCookies(ctx)
	}
	if err != nil {
		return nil, err
	}
	return gen.RemoveDevice204Response{}, nil
}
