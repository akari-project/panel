// SPDX-License-Identifier: AGPL-3.0-or-later

package clientapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/akari-project/panel/server/internal/apierr"
	"github.com/akari-project/panel/server/internal/clientapi/gen"
	"github.com/akari-project/panel/server/internal/clientconfig"
	"github.com/akari-project/panel/server/internal/db/sqlc"
)

// ConfigDeps 是 GET /v1/config 与最低版本检查所需的依赖（spec/30 API-03、API-11）。
type ConfigDeps struct {
	// Signer 为 PANEL_CONFIG_KEY（CONV-30）。
	Signer *clientconfig.Signer
	// AppName 为部署配置 client.app_name（已校验为 RFC 9110 token）。
	AppName string
	// APIEndpoints 为下发的接口根地址：主地址在前，备用地址在后。
	APIEndpoints []string
}

// UpgradeRequired 是自研客户端低于最低版本时的 426（API-03）。
var UpgradeRequired = apierr.New(http.StatusUpgradeRequired, "upgrade_required")

// configCache 保存最近一次签名的文档：输入不变时不重复签名（API-11）。
type configCache struct {
	mu      sync.Mutex
	payload string
	doc     []byte
	etag    string
}

// featuresWarn 记录最近一次告警的 settings 键 features 原始值的摘要：同一份异常值每个进程只告警一次，
// 不保存原文（spec/03 3.6、CONV-24）。
type featuresWarn struct {
	mu   sync.Mutex
	last [sha256.Size]byte
}

// GetConfig 返回已签名的客户端启动配置（spec/30 API-11）。签名确定，ETag 为文档字节的 SHA-256，
// 各副本一致，不需要共享缓存；响应带 Cache-Control: no-cache，由 If-None-Match 返回 304（CONV-13）。
func (s *Server) GetConfig(ctx context.Context, req gen.GetConfigRequestObject) (gen.GetConfigResponseObject, error) {
	if s.d.Config.Signer == nil {
		// cmd/panel 在 api 角色缺少 PANEL_CONFIG_KEY 时拒绝启动，这里只防御测试等直接构造的情形。
		return nil, errors.New("clientapi: PANEL_CONFIG_KEY not configured")
	}
	payload, err := s.configPayload(ctx)
	if err != nil {
		return nil, err
	}
	doc, etag, err := s.signConfig(payload)
	if err != nil {
		return nil, err
	}
	if req.Params.IfNoneMatch != nil && etagMatches(*req.Params.IfNoneMatch, etag) {
		return gen.GetConfig304Response{Headers: gen.NotModifiedResponseHeaders{ETag: &etag}}, nil
	}
	return signedConfigResponse{doc: doc, etag: etag}, nil
}

// signedConfigResponse 原样写出规范化的签名文档，使响应字节与 ETag 对应。
type signedConfigResponse struct {
	doc  []byte
	etag string
}

func (r signedConfigResponse) VisitGetConfigResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", r.etag)
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(r.doc)
	return err
}

func (s *Server) signConfig(payload map[string]any) ([]byte, string, error) {
	canon, err := clientconfig.Canonical(payload)
	if err != nil {
		return nil, "", err
	}
	c := &s.config
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.doc != nil && c.payload == string(canon) {
		return c.doc, c.etag, nil
	}
	d, err := s.d.Config.Signer.Sign(payload)
	if err != nil {
		return nil, "", err
	}
	c.payload, c.doc, c.etag = string(canon), d.Bytes, d.ETag
	return d.Bytes, d.ETag, nil
}

// configPayload 读取 payload 的输入（API-11）。
func (s *Server) configPayload(ctx context.Context) (map[string]any, error) {
	q := sqlc.New(s.d.Pool)
	policy := "open"
	if err := setting(ctx, q, "registration_policy", &policy); err != nil {
		return nil, err
	}
	if policy != "open" && policy != "invite_only" && policy != "closed" {
		policy = "open" // 不签发契约之外的取值；服务端注册时另行校验（AUTH-02）
	}
	raw, err := q.GetSetting(ctx, "features")
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	effective, invalid := clientconfig.Features(raw, clientconfig.Implemented)
	if invalid != nil {
		s.warnFeatures(ctx, raw, invalid)
	}
	features := map[string]any{}
	for m, on := range effective {
		features[m] = on
	}
	minVersion, err := minVersions(ctx, q)
	if err != nil {
		return nil, err
	}
	mv := map[string]any{}
	for p, v := range minVersion {
		mv[p] = v
	}
	issued, err := s.issuedAt(ctx, q)
	if err != nil {
		return nil, err
	}
	endpoints := s.d.Config.APIEndpoints
	if endpoints == nil {
		endpoints = []string{}
	}
	return map[string]any{
		"min_version":          mv,
		"announcement_version": int64(0), // 公告模块实现前为 0（API-11）
		"registration_policy":  policy,
		"features":             features,
		"api_endpoints":        endpoints,
		"issued_at":            issued,
	}, nil
}

// warnFeatures 记录 settings 键 features 中被当作关闭的异常值，只记键名（spec/03 3.6、CONV-24）。
func (s *Server) warnFeatures(ctx context.Context, raw []byte, invalid []string) {
	sum := sha256.Sum256(raw)
	w := &s.featuresWarn
	w.mu.Lock()
	seen := w.last == sum
	w.last = sum
	w.mu.Unlock()
	if !seen {
		s.d.Log.WarnContext(ctx, "settings features: invalid values treated as disabled", "keys", invalid)
	}
}

// issuedAt 返回 settings 键 config_issued_at。修改 features、registration_policy、min_version 的事务同时写入它；
// 站点尚未写入时由第一次请求用注入的时钟初始化（CONV-04、CONV-27），并发时先写入者生效，各副本读到同一值。
func (s *Server) issuedAt(ctx context.Context, q *sqlc.Queries) (string, error) {
	var v string
	err := setting(ctx, q, "config_issued_at", &v)
	if err != nil || v != "" {
		return v, err
	}
	now, _ := json.Marshal(s.d.Clock.Now().UTC().Truncate(time.Second).Format(time.RFC3339))
	if err := q.InitSetting(ctx, sqlc.InitSettingParams{Key: "config_issued_at", Value: now}); err != nil {
		return "", err
	}
	if err := setting(ctx, q, "config_issued_at", &v); err != nil {
		return "", err
	}
	if v == "" {
		return "", errors.New("config_issued_at not initialized")
	}
	return v, nil
}

// minVersions 读取 settings 键 min_version，忽略未知平台与格式错误的版本（保存时已校验，spec/03 3.6）。
func minVersions(ctx context.Context, q *sqlc.Queries) (map[string]string, error) {
	stored := map[string]string{}
	if err := setting(ctx, q, "min_version", &stored); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, p := range clientconfig.Platforms {
		if v, ok := stored[p]; ok && clientconfig.ValidVersion(v) {
			out[p] = v
		}
	}
	return out, nil
}

// setting 把设置项解码到 dst；不存在时 dst 保持默认值。
func setting(ctx context.Context, q *sqlc.Queries, key string, dst any) error {
	raw, err := q.GetSetting(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("settings %s: %w", key, err)
	}
	return nil
}

// etagMatches 按 RFC 9110 13.1.2 的弱比较判断 If-None-Match 是否命中。
func etagMatches(header, etag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == "*" || t == etag {
			return true
		}
	}
	return false
}

// checkVersion 对契约列出 426 的入口操作检查自研客户端的最低版本（API-03）。浏览器与第三方客户端不检查。
func (s *Server) checkVersion(ctx context.Context, r *http.Request) error {
	if s.ua == nil {
		return nil
	}
	c, ok := s.ua.Parse(r.UserAgent())
	if !ok || c.Platform == "" {
		return nil
	}
	min, err := minVersions(ctx, sqlc.New(s.d.Pool))
	if err != nil {
		return err
	}
	if clientconfig.Outdated(c, min) {
		return UpgradeRequired
	}
	return nil
}
