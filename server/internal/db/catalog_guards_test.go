// SPDX-License-Identifier: AGPL-3.0-or-later

package db_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/akari-project/panel/server/internal/testdb"
	"github.com/akari-project/panel/server/migrations"
)

// 00008：00007 的索引无效（并发建立失败）时，迁移中止并指出索引名（BIL-15，CONV-21）。
func TestCatalogIndexValidityCheck(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	src, err := fs.ReadFile(migrations.FS, "00008_catalog_guards.sql")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	begin := strings.Index(s, "DO $$")
	end := strings.Index(s[begin:], "END $$;")
	if begin < 0 || end < 0 {
		t.Fatal("validity check block not found")
	}
	check := s[begin : begin+end+len("END $$;")]
	mustExec(t, pool, check) // 迁移后两个索引有效
	mustExec(t, pool, `UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'plans_single_free'::regclass`)
	_, err = pool.Exec(ctx, check)
	if err == nil || !strings.Contains(err.Error(), "plans_single_free") {
		t.Fatalf("invalid index accepted: %v", err)
	}
}
