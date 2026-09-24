// SPDX-License-Identifier: AGPL-3.0-or-later

package testgateway

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Server 是运行在 httptest TLS 服务器上的测试用控制面端。
type Server struct {
	*Gateway
	URL       string       // https://127.0.0.1:<port>
	StreamURL string       // wss://127.0.0.1:<port>/v1/stream
	Client    *http.Client // 信任测试证书
	CAFile    string       // 测试证书（PEM），供外部进程（真实 Agent）使用

	srv *httptest.Server
}

// Start 启动测试用控制面端，测试结束时关闭。
func Start(t testing.TB, cfg Config) *Server {
	t.Helper()
	g := New(cfg)
	srv := httptest.NewUnstartedServer(g.Handler())
	srv.EnableHTTP2 = false // WebSocket 走 HTTP/1.1 升级
	srv.StartTLS()
	caFile := filepath.Join(t.TempDir(), "gateway-ca.pem")
	cert := srv.Certificate()
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Gateway:   g,
		URL:       srv.URL,
		StreamURL: "wss://" + strings.TrimPrefix(srv.URL, "https://") + "/v1/stream",
		Client:    srv.Client(),
		CAFile:    caFile,
		srv:       srv,
	}
	t.Cleanup(s.Close)
	return s
}

// Close 中断全部会话并关闭服务器。
func (s *Server) Close() {
	s.Gateway.Close()
	s.srv.Close()
}
