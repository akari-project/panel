// SPDX-License-Identifier: AGPL-3.0-or-later

package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
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

// GetConfig 返回已签名的客户端启动配置（spec/30 API-11）。签名确定，ETag 为文档字节的 SHA-256，
// 各副本一致，不需要共享缓存；响应带 Cache-Control: no-cache，由 If-None-Match 返回 304（CONV-13）。
func (s *Server) GetConfig(ctx context.Context, req gen.GetConfigRequestObject) (gen.GetConfigResponseObject, error) {
	if s.d.Config.Signer == nil {
		// cmd/panel 在 api 角色缺少 PANEL_CONFIG_KEY 时拒绝启动，这里只防御测试等直接构造的情形。
		return nil, errors.New("clientapi: PANEL_CONFIG_KEY not configured")
	}
	if s.d.Accounts == nil {
		// 有效注册策略由注册服务计算（AUTH-02）；app 总是注入，这里只防御直接构造的情形。
		return nil, errors.New("clientapi: account service not configured")
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

// configPayload 读取 payload 的输入（API-11）。各项都在同一个 q 上读取；settings 值异常时按 spec/03 3.6
// 取有效值，永不因此失败。
func (s *Server) configPayload(ctx context.Context) (map[string]any, error) {
	q := sqlc.New(s.d.Pool)
	// 有效注册策略与注册的服务端校验一致（AUTH-02）：任一注册控制键值异常时为 closed，告警由注册服务记录。
	rc, err := s.d.Accounts.RegistrationControl(ctx, q)
	if err != nil {
		return nil, err
	}
	raw, err := q.GetSetting(ctx, "features")
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	effective, invalid := clientconfig.Features(raw, clientconfig.ImplementedModules())
	s.warnSetting(ctx, "features", raw, invalid)
	features := map[string]any{}
	for m, on := range effective {
		features[m] = on
	}
	minVersion, err := s.minVersions(ctx, q)
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
		"registration_policy":  rc.Policy,
		"features":             features,
		"api_endpoints":        endpoints,
		"issued_at":            issued,
	}, nil
}

// warnSetting 记录 settings 键 key 中按 spec/03 3.6 取有效值的异常，只记键名（CONV-24）；同一份异常值
// 每个进程只告警一次，invalid 为空（值合法）时清除该键的告警记录。
func (s *Server) warnSetting(ctx context.Context, key string, raw []byte, invalid []string) {
	if s.settingsWarn.Changed(key, raw, invalid != nil) {
		s.d.Log.WarnContext(ctx, "settings: invalid values ignored", "keys", invalid)
	}
}

// issuedAt 返回 settings 键 config_issued_at。正常情况由站点初始化写入；features、registration_policy、
// min_version 的有效值变化时，在同一事务中以 sqlc BumpConfigIssuedAt 严格递增地更新（API-11）。这两条写入
// 路径分别属于站点初始化与设置管理接口（M1-09），尚未实现。键缺失时由第一次请求用注入的时钟惰性初始化，
// 这只是兜底（CONV-04、CONV-27），并发时先写入者生效，各副本读到同一值。
//
// 已存的值不是 YYYY-MM-DDTHH:MM:SSZ 字符串（直接改库，spec/03 3.6）时按缺键处理：记 warn 日志（只记键名），
// 仅当该行仍为这份异常值时用当前时刻覆盖，再读取生效的值；永不因此失败。
func (s *Server) issuedAt(ctx context.Context, q *sqlc.Queries) (string, error) {
	raw, err := q.GetSetting(ctx, "config_issued_at")
	missing := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !missing {
		return "", err
	}
	if v, ok := validIssuedAt(raw); ok {
		s.warnSetting(ctx, "config_issued_at", raw, nil)
		return v, nil
	}
	now := s.d.Clock.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	nowJSON, _ := json.Marshal(now)
	if missing {
		err = q.InitSetting(ctx, sqlc.InitSettingParams{Key: "config_issued_at", Value: nowJSON})
	} else {
		s.warnSetting(ctx, "config_issued_at", raw, []string{"config_issued_at"})
		_, err = q.ReplaceSettingIf(ctx, sqlc.ReplaceSettingIfParams{Key: "config_issued_at", Value: nowJSON, Old: raw})
	}
	if err != nil {
		return "", err
	}
	raw, err = q.GetSetting(ctx, "config_issued_at")
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if v, ok := validIssuedAt(raw); ok {
		return v, nil
	}
	// 读取之间又被写入异常值或删除：本次使用当前时刻，下次请求再纠正。
	return now, nil
}

var issuedAtRE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)

// validIssuedAt 判断 config_issued_at 的存储值是否为 YYYY-MM-DDTHH:MM:SSZ 形式的合法时刻（与 BumpConfigIssuedAt 的校验一致）。
func validIssuedAt(raw []byte) (string, bool) {
	var v string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &v) != nil || !issuedAtRE.MatchString(v) {
		return "", false
	}
	if _, err := time.Parse(time.RFC3339, v); err != nil {
		return "", false
	}
	return v, true
}

// minVersions 返回 settings 键 min_version 的有效值（spec/03 3.6、API-11），GET /v1/config 与 426 判断
// （API-03）共用：值不是对象时视为 {}，格式错误的版本与未知平台忽略。
func (s *Server) minVersions(ctx context.Context, q *sqlc.Queries) (map[string]string, error) {
	raw, err := q.GetSetting(ctx, "min_version")
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	versions, invalid := clientconfig.MinVersion(raw)
	s.warnSetting(ctx, "min_version", raw, invalid)
	return versions, nil
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
	min, err := s.minVersions(ctx, sqlc.New(s.d.Pool))
	if err != nil {
		return err
	}
	if clientconfig.Outdated(c, min) {
		return UpgradeRequired
	}
	return nil
}
