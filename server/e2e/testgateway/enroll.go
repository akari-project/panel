// SPDX-License-Identifier: AGPL-3.0-or-later

package testgateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"

	"google.golang.org/protobuf/proto"

	nodev1 "github.com/akari-project/panel-spec/gen/go/node/v1"

	"github.com/akari-project/panel/server/e2e/nodewire"
	"github.com/akari-project/panel/server/internal/logging"
)

// serveEnroll 实现 POST /v1/enrollments（NODE-02、NODE-18）：请求与响应为 protobuf，错误为 problem+json。
func (g *Gateway) serveEnroll(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		problem(w, http.StatusBadRequest, "invalid_request")
		return
	}
	req := new(nodev1.EnrollRequest)
	if err := proto.Unmarshal(body, req); err != nil || req.GetEnrollToken() == "" || req.GetHostFingerprint() == "" {
		problem(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := proto.Unmarshal(req.GetCapabilitiesRaw(), new(nodev1.Capabilities)); err != nil {
		problem(w, http.StatusBadRequest, "invalid_request")
		return
	}
	hash := sha256.Sum256([]byte(req.GetEnrollToken()))
	scheme := "wss"
	if r.TLS == nil {
		scheme = "ws"
	}
	streamURL := scheme + "://" + r.Host + "/v1/stream"

	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.cfg.Clock.Now()
	var n *node
	for _, c := range g.nodes {
		if c.tokenResp != nil || !c.tokenExpires.IsZero() {
			if subtle.ConstantTimeCompare(c.tokenHash[:], hash[:]) == 1 {
				n = c
				break
			}
		}
	}
	switch {
	case n == nil:
		problem(w, http.StatusNotFound, "not_found")
		return
	case n.tokenResp != nil:
		// 首次成功后 10 分钟内，同一令牌、同一主机指纹重复提交返回相同结果；其他情况 404。
		if req.GetHostFingerprint() == n.tokenFP && now.Sub(n.tokenUsedAt) <= g.cfg.EnrollRetryWindow {
			w.Header().Set("Content-Type", "application/x-protobuf")
			_, _ = w.Write(n.tokenResp)
			return
		}
		problem(w, http.StatusNotFound, "not_found")
		return
	case !now.Before(n.tokenExpires):
		problem(w, http.StatusNotFound, "not_found")
		return
	}
	psk := make([]byte, nodewire.PSKSize)
	if _, err := io.ReadFull(g.cfg.Rand, psk); err != nil {
		problem(w, http.StatusInternalServerError, "internal")
		return
	}
	resp, err := proto.Marshal(&nodev1.EnrollResponse{
		NodeId:       n.id,
		Psk:          psk,
		StreamUrl:    streamURL,
		ProtoVersion: g.cfg.ProtoMax,
	})
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal")
		return
	}
	g.setPSKLocked(n, psk)
	n.tokenUsedAt, n.tokenFP, n.tokenResp = now, req.GetHostFingerprint(), resp
	g.cfg.Logger.Info("testgateway: node enrolled", logging.KeyNodeID, n.id)
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(resp)
}

func problem(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":       "about:blank",
		"title":      http.StatusText(status),
		"status":     status,
		"code":       code,
		"request_id": "testgateway",
	})
}
