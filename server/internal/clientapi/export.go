// SPDX-License-Identifier: AGPL-3.0-or-later

package clientapi

import (
	"context"

	"github.com/akari-project/panel/server/internal/auth"
	"github.com/akari-project/panel/server/internal/clientapi/gen"
)

// exportNoStore 是导入链接响应的 Cache-Control（契约 getExportLink、rotateExportLink）。
var exportNoStore = "private, no-store"

// exportURL 是导入链接：接口主地址（spec/30 API-11）+ /v1/configurations/ + 令牌。链接等同凭据，不写日志（CONV-24）。
func (s *Server) exportURL(token string) string {
	return s.d.ExportBaseURL + "/v1/configurations/" + token
}

// GetExportLink 返回当前的第三方客户端导入链接（AUTH-16）。created_at 为令牌最近一次生成或重置的时刻。
func (s *Server) GetExportLink(ctx context.Context, _ gen.GetExportLinkRequestObject) (gen.GetExportLinkResponseObject, error) {
	p, _ := auth.FromContext(ctx)
	t, err := s.d.Accounts.ExportToken(ctx, p.AccountID)
	if err != nil {
		return nil, err
	}
	return gen.GetExportLink200JSONResponse{
		Body:    gen.ExportLink{Url: s.exportURL(t.Token), CreatedAt: t.RotatedAt},
		Headers: gen.GetExportLink200ResponseHeaders{CacheControl: &exportNoStore},
	}, nil
}

// RotateExportLink 重置导入链接（AUTH-16）：需要重新验证（AUTH-23），未通过时不写库；响应含秘密值，
// 不接受 Idempotency-Key（CONV-12）。
func (s *Server) RotateExportLink(ctx context.Context, _ gen.RotateExportLinkRequestObject) (gen.RotateExportLinkResponseObject, error) {
	p, _ := auth.FromContext(ctx)
	if err := s.d.Sessions.RequireRecentAuth(ctx, p); err != nil {
		return nil, err
	}
	t, err := s.d.Accounts.RotateExportToken(ctx, p.AccountID)
	if err != nil {
		return nil, err
	}
	return gen.RotateExportLink200JSONResponse{
		Body:    gen.ExportLink{Url: s.exportURL(t.Token), CreatedAt: t.RotatedAt},
		Headers: gen.RotateExportLink200ResponseHeaders{CacheControl: &exportNoStore},
	}, nil
}
