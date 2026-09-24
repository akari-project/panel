// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/akari-project/panel/server/migrations"
)

// CONV-21：文件名 NNNNN_描述.sql，版本连续，没有 Down 段；CONV-25：SPDX 头在前两行内。
func TestMigrationFiles(t *testing.T) {
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("no migrations embedded")
	}
	down := regexp.MustCompile(`(?m)^\s*--\s*\+goose\s+Down\b`)
	for i, n := range names {
		m := migrationName.FindStringSubmatch(n)
		if m == nil {
			t.Errorf("%s: name does not match NNNNN_description.sql", n)
			continue
		}
		if want := fmt.Sprintf("%05d", i+1); m[1] != want {
			t.Errorf("%s: version gap, want %s", n, want)
		}
		b, _ := fs.ReadFile(migrations.FS, n)
		src := string(b)
		if down.MatchString(src) {
			t.Errorf("%s: contains a -- +goose Down section (CONV-21)", n)
		}
		head := strings.SplitN(src, "\n", 3)
		// 拆开字面量，避免 REUSE 把这一行当作本文件的 SPDX 标识解析。
		if len(head) < 2 || !strings.Contains(head[0]+head[1], "SPDX-License-"+"Identifier: AGPL-3.0-or-later") {
			t.Errorf("%s: missing SPDX header in the first two lines (CONV-25)", n)
		}
		if !strings.Contains(src, "-- +goose Up") {
			t.Errorf("%s: missing -- +goose Up", n)
		}
	}
	v, err := RequiredVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != int64(len(names)) {
		t.Errorf("RequiredVersion = %d, want %d", v, len(names))
	}
}

// CONV-27：业务时间列不得有 DEFAULT now()；只有 created_at 与 updated_at 可以。
func TestNoBusinessTimeDefaults(t *testing.T) {
	names, _ := fs.Glob(migrations.FS, "*.sql")
	col := regexp.MustCompile(`(?m)^\s*(\w+)\s+timestamptz\b[^\n]*DEFAULT\s+now\(\)`)
	for _, n := range names {
		b, _ := fs.ReadFile(migrations.FS, n)
		for _, m := range col.FindAllStringSubmatch(string(b), -1) {
			if m[1] != "created_at" && m[1] != "updated_at" {
				t.Errorf("%s: column %s has DEFAULT now(); business time must be written by the injected clock (CONV-27)", n, m[1])
			}
		}
	}
}
