// SPDX-License-Identifier: AGPL-3.0-or-later

package realpanel

import (
	"context"
	"testing"

	"github.com/akari-project/panel/server/internal/testdb"
)

// 种子数据满足数据库约束，可以重复执行。
func TestSeedCatalog(t *testing.T) {
	pool := testdb.New(t)
	SeedCatalog(t, pool)
	SeedCatalog(t, pool)
	var plans, onSale, prices, groups int
	if err := pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM plans), (SELECT count(*) FROM plans WHERE status = 'on_sale'),
		(SELECT count(*) FROM plan_prices WHERE on_sale), (SELECT count(*) FROM location_groups)`).Scan(&plans, &onSale, &prices, &groups); err != nil {
		t.Fatal(err)
	}
	if plans != 3 || onSale != 2 || prices != 4 || groups != 2 {
		t.Fatalf("plans %d on_sale %d prices %d groups %d", plans, onSale, prices, groups)
	}
}
