// SPDX-License-Identifier: AGPL-3.0-or-later

package catalog

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/audit"
	"github.com/akari-project/panel/server/internal/db/sqlc"
)

// Group 是线路组（ACS-05、ACS-06）。
type Group struct {
	ID          uuid.UUID
	Name        string
	Description *string
	// MinTier 为空表示不限等级。
	MinTier   *int32
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
	// HostCount 是成员节点数。
	HostCount int32
	PlanIDs   []uuid.UUID
}

// GroupFields 是创建或修改线路组时提交的字段。
type GroupFields struct {
	Name        Opt[string]
	Description Opt[*string]
	MinTier     Opt[*int]
}

func (g GroupFields) check(f *fields) {
	if g.Name.Set {
		f.name("name", g.Name.V)
	}
	if g.MinTier.Set && g.MinTier.V != nil {
		f.int32Range("min_tier", *g.MinTier.V, 0)
	}
}

func fillGroups(ctx context.Context, q *sqlc.Queries, groups []Group) error {
	if len(groups) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(groups))
	idx := make(map[uuid.UUID]int, len(groups))
	for i, g := range groups {
		ids[i] = g.ID
		idx[g.ID] = i
	}
	links, err := q.GroupPlanIDs(ctx, ids)
	if err != nil {
		return err
	}
	for _, l := range links {
		g := &groups[idx[l.GroupID]]
		g.PlanIDs = append(g.PlanIDs, l.PlanID)
	}
	return nil
}

func groupFromRow(r sqlc.GetLocationGroupRow) Group {
	return Group{ID: r.ID, Name: r.Name, Description: r.Description, MinTier: r.MinTier, Version: r.Version,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, HostCount: r.HostCount, PlanIDs: []uuid.UUID{}}
}

func getGroup(ctx context.Context, q *sqlc.Queries, id uuid.UUID) (Group, error) {
	r, err := q.GetLocationGroup(ctx, id)
	if err != nil {
		return Group{}, notFound(err)
	}
	out := []Group{groupFromRow(r)}
	if err := fillGroups(ctx, q, out); err != nil {
		return Group{}, err
	}
	return out[0], nil
}

// GroupKey 是线路组列表的游标键（CONV-11）。
type GroupKey struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// ListGroups 按 (created_at, id) 列出线路组，after 为上一页最后一项的键。
func (s *Service) ListGroups(ctx context.Context, after *GroupKey, max int32) ([]Group, error) {
	q := sqlc.New(s.Pool)
	params := sqlc.ListLocationGroupsParams{MaxRows: max}
	if after != nil {
		params.CursorAt, params.CursorID = &after.CreatedAt, &after.ID
	}
	rows, err := q.ListLocationGroups(ctx, params)
	if err != nil {
		return nil, err
	}
	out := make([]Group, len(rows))
	for i, r := range rows {
		out[i] = groupFromRow(sqlc.GetLocationGroupRow(r))
	}
	return out, fillGroups(ctx, q, out)
}

// GetGroup 返回线路组；不存在返回 404。
func (s *Service) GetGroup(ctx context.Context, id uuid.UUID) (Group, error) {
	return getGroup(ctx, sqlc.New(s.Pool), id)
}

// nameTaken 把线路组名称唯一约束的冲突转为 400 name taken（管理接口可以使用 taken，CONV-16）。
func nameTaken(err error) error {
	if code, constraint := pgCode(err); code == "23505" && constraint == "location_groups_name_key" {
		return apierr.Invalid(apierr.Field("name", "taken"))
	}
	return err
}

func groupAudit(name string, desc *string, minTier *int32) map[string]any {
	return map[string]any{"name": name, "description": desc, "min_tier": minTier}
}

// CreateGroup 创建线路组。名称已被占用返回 400 name taken。
func (s *Service) CreateGroup(ctx context.Context, in GroupFields) (Group, error) {
	var f fields
	if !in.Name.Set {
		f.add("name", "required")
	}
	in.check(&f)
	if err := f.err(); err != nil {
		return Group{}, err
	}
	desc, minTier := in.Description.or(nil), ptr32(in.MinTier.or(nil))
	var out Group
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		id, err := q.InsertLocationGroup(ctx, sqlc.InsertLocationGroupParams{Name: in.Name.V, Description: desc, MinTier: minTier})
		if err != nil {
			return nameTaken(err)
		}
		if err := audit.Record(ctx, q, audit.Entry{Action: "location_group.create", TargetType: "location_group",
			TargetID: id.String(), Diff: audit.Values(groupAudit(in.Name.V, desc, minTier))}); err != nil {
			return err
		}
		out, err = getGroup(ctx, q, id)
		return err
	})
	return out, err
}

func lockGroup(ctx context.Context, q *sqlc.Queries, id uuid.UUID, ifMatch string) (sqlc.LockLocationGroupRow, error) {
	r, err := q.LockLocationGroup(ctx, id)
	if err != nil {
		return r, notFound(err)
	}
	return r, checkIfMatch(ifMatch, ETag(r.Version))
}

// UpdateGroup 修改线路组；min_tier 变化写 location_group.changed（ACS-05）。没有实际变化时不修改版本、不写审计。
func (s *Service) UpdateGroup(ctx context.Context, id uuid.UUID, ifMatch string, in GroupFields) (Group, error) {
	if !in.Name.Set && !in.Description.Set && !in.MinTier.Set {
		return Group{}, apierr.Invalid()
	}
	var f fields
	in.check(&f)
	if err := f.err(); err != nil {
		return Group{}, err
	}
	var out Group
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		cur, err := lockGroup(ctx, q, id, ifMatch)
		if err != nil {
			return err
		}
		name, desc, minTier := in.Name.or(cur.Name), in.Description.or(cur.Description), cur.MinTier
		if in.MinTier.Set {
			minTier = ptr32(in.MinTier.V)
		}
		changes := audit.Changes(groupAudit(cur.Name, cur.Description, cur.MinTier), groupAudit(name, desc, minTier))
		if len(changes) > 0 {
			if err := q.UpdateLocationGroup(ctx, sqlc.UpdateLocationGroupParams{ID: id, Name: name, Description: desc,
				MinTier: minTier}); err != nil {
				return nameTaken(err)
			}
			if _, ok := changes["min_tier"]; ok {
				if err := emit(ctx, q, "location_group.changed", map[string]any{"location_group_id": id}); err != nil {
					return err
				}
			}
			if err := audit.Record(ctx, q, audit.Entry{Action: "location_group.update", TargetType: "location_group",
				TargetID: id.String(), Diff: changes}); err != nil {
				return err
			}
		}
		out, err = getGroup(ctx, q, id)
		return err
	})
	return out, err
}

// DeleteGroup 删除线路组。仍被套餐引用或仍有节点成员时返回 409 invalid_state（ACS-06）。
func (s *Service) DeleteGroup(ctx context.Context, id uuid.UUID, ifMatch string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		cur, err := lockGroup(ctx, q, id, ifMatch)
		if err != nil {
			return err
		}
		inUse, err := q.LocationGroupInUse(ctx, id)
		if err != nil {
			return err
		}
		if inUse {
			return apierr.InvalidState
		}
		if err := q.DeleteLocationGroup(ctx, id); err != nil {
			// 锁定线路组之后，新的关联因外键检查需要等待本事务；这里只作为兜底。
			if code, _ := pgCode(err); code == "23503" {
				return apierr.InvalidState
			}
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Action: "location_group.delete", TargetType: "location_group",
			TargetID: id.String(), Diff: audit.Values(map[string]any{"name": cur.Name, "min_tier": cur.MinTier})})
	})
}
