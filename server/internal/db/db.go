// SPDX-License-Identifier: AGPL-3.0-or-later

// Package db 负责 PostgreSQL 连接池、迁移与数据库版本校验（spec/40 DEP-12）。
// 数据访问代码由 sqlc 从 queries/*.sql 生成到 sqlc/，不手改。
package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/akari-project/panel/server/migrations"
)

// Open 建立连接池并确认数据库可达。
func Open(ctx context.Context, url string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("db: parse url: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// newProvider 返回基于嵌入迁移的 goose provider。goose 的 PostgreSQL 会话锁保证
// 多个实例同时执行 migrate 时串行进行（DEP-12）。
func newProvider(pool *pgxpool.Pool, fsys fs.FS) (*goose.Provider, *sql.DB, error) {
	sqlDB := stdlib.OpenDBFromPool(pool)
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		sqlDB.Close()
		return nil, nil, fmt.Errorf("db: goose locker: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fsys, goose.WithSessionLocker(locker))
	if err != nil {
		sqlDB.Close()
		return nil, nil, fmt.Errorf("db: goose provider: %w", err)
	}
	return p, sqlDB, nil
}

// Migrate 执行全部未应用的迁移，返回迁移后的版本。
func Migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) (int64, error) {
	return migrateFS(ctx, pool, migrations.FS, log)
}

func migrateFS(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, log *slog.Logger) (int64, error) {
	p, sqlDB, err := newProvider(pool, fsys)
	if err != nil {
		return 0, err
	}
	defer sqlDB.Close()
	results, err := p.Up(ctx)
	for _, r := range results {
		if log != nil && r.Error == nil {
			log.Info("migration applied", "version", r.Source.Version, "file", r.Source.Path, "duration_ms", r.Duration.Milliseconds())
		}
	}
	if err != nil {
		return 0, fmt.Errorf("db: migrate: %w", err)
	}
	return p.GetDBVersion(ctx)
}

// MigrationStatus 描述一个迁移的状态。
type MigrationStatus struct {
	Version   int64
	Source    string
	IsApplied bool
}

// Status 返回每个嵌入迁移的应用状态。
func Status(ctx context.Context, pool *pgxpool.Pool) ([]MigrationStatus, error) {
	p, sqlDB, err := newProvider(pool, migrations.FS)
	if err != nil {
		return nil, err
	}
	defer sqlDB.Close()
	st, err := p.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: status: %w", err)
	}
	out := make([]MigrationStatus, 0, len(st))
	for _, s := range st {
		out = append(out, MigrationStatus{Version: s.Source.Version, Source: s.Source.Path, IsApplied: s.State == goose.StateApplied})
	}
	return out, nil
}

// migrationName 是迁移文件名格式 NNNNN_描述.sql（CONV-21）。
var migrationName = regexp.MustCompile(`^(\d{5})_[\p{L}\p{N}_]+\.sql$`)

// RequiredVersion 返回本二进制内嵌的最高迁移版本，即运行所需的最低数据库版本。
func RequiredVersion() (int64, error) {
	return requiredVersionFS(migrations.FS)
}

func requiredVersionFS(fsys fs.FS) (int64, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return 0, err
	}
	var max int64
	for _, n := range names {
		m := migrationName.FindStringSubmatch(n)
		if m == nil {
			return 0, fmt.Errorf("db: migration %q does not match NNNNN_description.sql", n)
		}
		v, _ := strconv.ParseInt(m[1], 10, 64)
		max = maxInt64(max, v)
	}
	return max, nil
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ErrSchemaTooOld 表示数据库版本低于二进制所需。
var ErrSchemaTooOld = errors.New("db: database schema is older than this binary requires; run `panel migrate` first")

// CheckVersion 校验数据库版本不低于编译时的要求。api、gateway、worker 启动时调用，不自动迁移（DEP-12）。
func CheckVersion(ctx context.Context, pool *pgxpool.Pool) (current, required int64, err error) {
	required, err = RequiredVersion()
	if err != nil {
		return 0, 0, err
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('goose_db_version') IS NOT NULL`).Scan(&exists); err != nil {
		return 0, required, fmt.Errorf("db: read schema version: %w", err)
	}
	if exists {
		if err := pool.QueryRow(ctx, `SELECT coalesce(max(version_id), 0) FROM goose_db_version WHERE is_applied`).Scan(&current); err != nil {
			return 0, required, fmt.Errorf("db: read schema version: %w", err)
		}
	}
	if current < required {
		return current, required, fmt.Errorf("%w (database %d, required %d)", ErrSchemaTooOld, current, required)
	}
	return current, required, nil
}

// MigrationsDigest 返回全部嵌入迁移内容的 SHA-256（十六进制），用于识别测试模板库是否过期。
func MigrationsDigest() string {
	names, _ := fs.Glob(migrations.FS, "*.sql")
	h := sha256.New()
	for _, n := range names {
		b, _ := fs.ReadFile(migrations.FS, n)
		fmt.Fprintf(h, "%s\x00%d\x00", n, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}
