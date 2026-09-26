// SPDX-License-Identifier: AGPL-3.0-or-later

package consoleapi

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/billing/catalog"
	"github.com/akari-project/panel/server/internal/consoleapi/gen"
)

func groupAPI(g catalog.Group) gen.LocationGroup {
	return gen.LocationGroup{Id: g.ID, Name: g.Name, Description: nullableOf(g.Description, func(s string) string { return s }),
		MinTier: nullableOf(g.MinTier, toInt), HostCount: g.HostCount, PlanIds: g.PlanIDs,
		CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt}
}

func groupETag(g catalog.Group) *string {
	e := catalog.ETag(g.Version)
	return &e
}

// ListLocationGroups 按 (created_at, id) 分页列出线路组（CONV-11）。
func (s *Server) ListLocationGroups(ctx context.Context, req gen.ListLocationGroupsRequestObject) (gen.ListLocationGroupsResponseObject, error) {
	limit, err := pageLimit(req.Params.Limit)
	if err != nil {
		return nil, err
	}
	var cur struct {
		CreatedAt time.Time `json:"t"`
		ID        uuid.UUID `json:"id"`
	}
	var after *catalog.GroupKey
	if ok, err := decodeCursor(req.Params.Cursor, &cur); err != nil {
		return nil, err
	} else if ok {
		after = &catalog.GroupKey{CreatedAt: cur.CreatedAt, ID: cur.ID}
	}
	groups, err := s.catalog.ListGroups(ctx, after, limit+1)
	if err != nil {
		return nil, err
	}
	var next *string
	if len(groups) > int(limit) {
		groups = groups[:limit]
		last := groups[len(groups)-1]
		next = encodeCursor(map[string]any{"t": last.CreatedAt, "id": last.ID})
	}
	resp := gen.ListLocationGroups200JSONResponse{Items: make([]gen.LocationGroup, len(groups)), NextCursor: nullableString(next)}
	for i, g := range groups {
		resp.Items[i] = groupAPI(g)
	}
	return resp, nil
}

// CreateLocationGroup 创建线路组。
func (s *Server) CreateLocationGroup(ctx context.Context, req gen.CreateLocationGroupRequestObject) (gen.CreateLocationGroupResponseObject, error) {
	if req.Body == nil {
		return nil, apierr.Invalid()
	}
	g, err := s.catalog.CreateGroup(ctx, catalog.GroupFields{Name: catalog.Some(req.Body.Name),
		Description: optNullable(req.Body.Description), MinTier: optNullable(req.Body.MinTier)})
	if err != nil {
		return nil, err
	}
	return gen.CreateLocationGroup201JSONResponse{Body: groupAPI(g), Headers: gen.CreateLocationGroup201ResponseHeaders{ETag: groupETag(g)}}, nil
}

// GetLocationGroup 返回线路组详情，带强 ETag，支持 If-None-Match（CONV-13）。
func (s *Server) GetLocationGroup(ctx context.Context, req gen.GetLocationGroupRequestObject) (gen.GetLocationGroupResponseObject, error) {
	g, err := s.catalog.GetGroup(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	etag := groupETag(g)
	if req.Params.IfNoneMatch != nil && *req.Params.IfNoneMatch == *etag {
		return gen.GetLocationGroup304Response{Headers: gen.NotModifiedResponseHeaders{ETag: etag}}, nil
	}
	return gen.GetLocationGroup200JSONResponse{Body: groupAPI(g), Headers: gen.GetLocationGroup200ResponseHeaders{ETag: etag}}, nil
}

// UpdateLocationGroup 修改线路组；min_tier 变化写 location_group.changed（ACS-05）。必须携带 If-Match（CONV-28）。
func (s *Server) UpdateLocationGroup(ctx context.Context, req gen.UpdateLocationGroupRequestObject) (gen.UpdateLocationGroupResponseObject, error) {
	if req.Body == nil {
		return nil, apierr.Invalid()
	}
	g, err := s.catalog.UpdateGroup(ctx, req.Id, ifMatch(req.Params.IfMatch), catalog.GroupFields{Name: opt(req.Body.Name),
		Description: optNullable(req.Body.Description), MinTier: optNullable(req.Body.MinTier)})
	if err != nil {
		return nil, err
	}
	return gen.UpdateLocationGroup200JSONResponse{Body: groupAPI(g), Headers: gen.UpdateLocationGroup200ResponseHeaders{ETag: groupETag(g)}}, nil
}

// DeleteLocationGroup 删除线路组；仍被套餐引用或仍有节点返回 409 invalid_state（ACS-06）。必须携带 If-Match（CONV-28）。
func (s *Server) DeleteLocationGroup(ctx context.Context, req gen.DeleteLocationGroupRequestObject) (gen.DeleteLocationGroupResponseObject, error) {
	if err := s.catalog.DeleteGroup(ctx, req.Id, ifMatch(req.Params.IfMatch)); err != nil {
		return nil, err
	}
	return gen.DeleteLocationGroup204Response{}, nil
}

// PreviewLocationGroupImpact 计算线路组拟议变更的影响（CON-07），不产生副作用。
func (s *Server) PreviewLocationGroupImpact(ctx context.Context, req gen.PreviewLocationGroupImpactRequestObject) (gen.PreviewLocationGroupImpactResponseObject, error) {
	if req.Body == nil {
		return nil, apierr.Invalid()
	}
	in := catalog.GroupProposal{MinTier: optNullable(req.Body.MinTier)}
	if req.Body.AddHostIds != nil {
		in.AddHosts = *req.Body.AddHostIds
	}
	if req.Body.RemoveHostIds != nil {
		in.RemoveHosts = *req.Body.RemoveHostIds
	}
	if req.Body.IsDeletion != nil {
		in.IsDeletion = *req.Body.IsDeletion
	}
	out, err := s.catalog.PreviewGroup(ctx, req.Id, in)
	if err != nil {
		return nil, err
	}
	return gen.PreviewLocationGroupImpact200JSONResponse(impactAPI(out)), nil
}
