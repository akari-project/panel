// SPDX-License-Identifier: AGPL-3.0-or-later

package realpanel

import (
	"context"
	_ "embed"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SeedCatalogSQL 是开发与端到端测试用的套餐目录（seed_catalog.sql）：1 个免费套餐、2 个在售付费套餐、2 个线路组。
//
//go:embed seed_catalog.sql
var SeedCatalogSQL string

// 种子数据中的固定 ID。
const (
	SeedFreePlanID     = "0192f0c4-1a00-7000-8000-00000000a000"
	SeedStandardPlanID = "0192f0c4-1a00-7000-8000-00000000a001"
	SeedProPlanID      = "0192f0c4-1a00-7000-8000-00000000a002"
	SeedAsiaGroupID    = "0192f0c4-1a00-7000-8000-00000000b001"
	SeedGlobalGroupID  = "0192f0c4-1a00-7000-8000-00000000b002"
)

// SeedCatalog 写入种子套餐目录，可重复执行。
func SeedCatalog(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), SeedCatalogSQL); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
}
