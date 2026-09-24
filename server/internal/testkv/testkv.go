// SPDX-License-Identifier: AGPL-3.0-or-later

// Package testkv 是集成测试的 Valkey 基座（spec/42 42.3），用法与 testdb 相同。
//
// 每次 go test 启动一个 Valkey 9 容器（与 compose.dev.yaml 一致），全部测试共享。
// 被测代码的键都含账号、会话或随机值，测试之间不会互相干扰，因此不清空数据库。
//
// 环境变量：
//   - PANEL_TEST_VALKEY_URL：使用已有的 Valkey 代替容器；
//   - PANEL_REQUIRE_DB=1：Valkey 不可用时测试失败而不是跳过（CI 中设置）。
package testkv

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/valkey-io/valkey-go"

	"github.com/akari-project/panel/server/internal/kv"
)

// Image 是测试使用的 Valkey 镜像。
const Image = "valkey/valkey:9"

var (
	once     sync.Once
	url      string
	setupErr error
)

// New 返回一个 Valkey 客户端，测试结束时关闭。
func New(t testing.TB) valkey.Client {
	t.Helper()
	c, _ := NewWithURL(t)
	return c
}

// NewWithURL 与 New 相同，另外返回连接 URL。
func NewWithURL(t testing.TB) (valkey.Client, string) {
	t.Helper()
	once.Do(setup)
	if setupErr != nil {
		if os.Getenv("PANEL_REQUIRE_DB") == "1" {
			t.Fatalf("testkv: %v", setupErr)
		}
		t.Skipf("testkv: Valkey unavailable, skipping (set PANEL_REQUIRE_DB=1 to fail instead): %v", setupErr)
	}
	c, err := kv.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("testkv: %v", err)
	}
	t.Cleanup(c.Close)
	return c, url
}

func setup() {
	if u := os.Getenv("PANEL_TEST_VALKEY_URL"); u != "" {
		url = u
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var lastErr error
	for attempt := 1; attempt <= 5; attempt++ {
		u, err := start(ctx)
		if err == nil {
			url = u
			return
		}
		lastErr = err
		select {
		case <-ctx.Done():
			setupErr = lastErr
			return
		case <-time.After(time.Duration(attempt) * time.Second):
		}
	}
	setupErr = lastErr
}

// start 启动（或复用）本次 go test 的 Valkey 容器，命名方式与 testdb 相同。
func start(ctx context.Context) (u string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("start container: %v", r)
		}
	}()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        Image,
			Name:         fmt.Sprintf("panel-testkv-%d", os.Getppid()),
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(time.Minute),
		},
		Started: true,
		Reuse:   true,
	})
	if err != nil {
		return "", fmt.Errorf("start container: %w", err)
	}
	ep, err := c.PortEndpoint(ctx, "6379/tcp", "redis")
	if err != nil {
		return "", err
	}
	return ep, nil
}
