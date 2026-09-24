// SPDX-License-Identifier: AGPL-3.0-or-later

// Package testdb 是集成测试的 PostgreSQL 18 基座（spec/42 42.3）。
//
// 每个测试进程启动一个 PostgreSQL 18 容器（testcontainers），执行全部迁移生成模板库，
// 每个测试从模板库复制出独立的数据库，测试之间互不影响，可以并行。
//
// 环境变量：
//   - PANEL_TEST_DATABASE_URL：使用已有的 PostgreSQL 18（需要建库权限）代替容器；
//   - PANEL_REQUIRE_DB=1：数据库不可用时测试失败而不是跳过（CI 中设置）。
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/akari-project/panel/server/internal/db"
)

// Image 是测试使用的 PostgreSQL 镜像，与 compose.dev.yaml 一致。
const Image = "postgres:18"

const templateDB = "panel_test_template"

var (
	once     sync.Once
	adminURL string // 连接到维护库（postgres）的 URL
	setupErr error
)

// New 返回一个已执行全部迁移的全新数据库的连接池，测试结束时删除该数据库。
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool, _ := NewWithURL(t)
	return pool
}

// NewWithURL 与 New 相同，另外返回该数据库的连接 URL（用于测试子命令等需要 URL 的场景）。
func NewWithURL(t testing.TB) (*pgxpool.Pool, string) {
	t.Helper()
	once.Do(setup)
	if setupErr != nil {
		if os.Getenv("PANEL_REQUIRE_DB") == "1" {
			t.Fatalf("testdb: %v", setupErr)
		}
		t.Skipf("testdb: PostgreSQL unavailable, skipping (set PANEL_REQUIRE_DB=1 to fail instead): %v", setupErr)
	}

	ctx := context.Background()
	name := "t_" + randomHex(8)
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("testdb: connect: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, templateDB)); err != nil {
		t.Fatalf("testdb: create database: %v", err)
	}
	dbURL := withDatabase(adminURL, name)
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("testdb: open: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(context.Background(), adminURL)
		if err != nil {
			t.Logf("testdb: cleanup connect: %v", err)
			return
		}
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name)); err != nil {
			t.Logf("testdb: drop %s: %v", name, err)
		}
	})
	return pool, dbURL
}

func setup() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if u := os.Getenv("PANEL_TEST_DATABASE_URL"); u != "" {
		adminURL = u
	} else {
		u, err := startContainer(ctx)
		if err != nil {
			setupErr = err
			return
		}
		adminURL = u
	}
	setupErr = buildTemplate(ctx)
}

// startContainer 启动（或复用）本次 go test 的 PostgreSQL 容器。go test 把各包的测试进程
// 作为同一个父进程的子进程并行运行，容器以父进程 PID 命名并复用，全部包共享一个实例；
// testcontainers 的 reaper 在父进程结束后清理它。并行启动时 reaper 可能发生名称冲突，重试即可。
func startContainer(ctx context.Context) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= 5; attempt++ {
		u, err := tryStartContainer(ctx)
		if err == nil {
			return u, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return "", lastErr
		case <-time.After(time.Duration(attempt) * time.Second):
		}
	}
	return "", lastErr
}

func tryStartContainer(ctx context.Context) (u string, err error) {
	// 没有 Docker 时 testcontainers 可能 panic，转为错误以便跳过。
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("start container: %v", r)
		}
	}()
	c, err := tcpostgres.Run(ctx, Image,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("panel"),
		tcpostgres.WithPassword("panel"),
		testcontainers.WithReuseByName(fmt.Sprintf("panel-testdb-%d", os.Getppid())),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		return "", fmt.Errorf("start container: %w", err)
	}
	return c.ConnectionString(ctx, "sslmode=disable")
}

// buildTemplate 在模板库上执行全部迁移。同一 PostgreSQL 上的多个测试进程
// （go test 按包并行）用 advisory lock 串行建模板，模板已是最新版本时直接复用。
func buildTemplate(ctx context.Context) error {
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock(7263001)"); err != nil {
		return err
	}
	defer admin.Exec(context.Background(), "SELECT pg_advisory_unlock(7263001)")

	required, err := db.RequiredVersion()
	if err != nil {
		return err
	}
	var exists bool
	if err := admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", templateDB).Scan(&exists); err != nil {
		return err
	}
	if exists {
		pool, err := pgxpool.New(ctx, withDatabase(adminURL, templateDB))
		if err != nil {
			return err
		}
		_, _, verr := db.CheckVersion(ctx, pool)
		pool.Close()
		if verr == nil && !migrationsChanged(ctx, admin) {
			return nil
		}
		if _, err := admin.Exec(ctx, "DROP DATABASE "+templateDB+" WITH (FORCE)"); err != nil {
			return err
		}
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+templateDB); err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, withDatabase(adminURL, templateDB))
	if err != nil {
		return err
	}
	defer pool.Close()
	v, err := db.Migrate(ctx, pool, nil)
	if err != nil {
		return err
	}
	if v != required {
		return fmt.Errorf("template migrated to %d, want %d", v, required)
	}
	if _, err := pool.Exec(ctx, "COMMENT ON DATABASE "+templateDB+" IS '"+db.MigrationsDigest()+"'"); err != nil {
		return err
	}
	return nil
}

// migrationsChanged 报告模板库是否由内容不同的迁移生成（开发中修改尚未提交的迁移时）。
func migrationsChanged(ctx context.Context, admin *pgx.Conn) bool {
	var comment *string
	err := admin.QueryRow(ctx, "SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1", templateDB).Scan(&comment)
	return err != nil || comment == nil || *comment != db.MigrationsDigest()
}

func withDatabase(raw, name string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Path = "/" + strings.TrimPrefix(name, "/")
	return u.String()
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
