// SPDX-License-Identifier: AGPL-3.0-or-later

package clientapi

import (
	"context"
	"net/http"
	"net/netip"
)

// reqInfo 是 strict 处理器需要、但请求对象中没有的请求信息。
type reqInfo struct {
	// IP 是按可信代理规则解析的客户端地址（DEP-13）。只用于限流与 /24、/48 前缀，不写日志（CONV-24）。
	IP             netip.Addr
	UserAgent      string
	AcceptLanguage string
	// Body 是请求体原文，只为 oneOf 请求体的操作保存（见 rawBodyOps）。
	Body   []byte
	r      *http.Request
	header http.Header
}

// rawBodyOps 是请求体为 oneOf 的操作。oapi-codegen 的 strict 模式把这类请求体解码为不带
// UnmarshalJSON 的新类型，内容会丢失，因此由路由保存原文，处理器自行解码。
var rawBodyOps = map[string]bool{"createSession": true, "reauthenticate": true}

type reqInfoKey struct{}

func withReqInfo(ctx context.Context, w http.ResponseWriter, r *http.Request, ip netip.Addr) context.Context {
	return context.WithValue(ctx, reqInfoKey{}, &reqInfo{
		IP: ip, UserAgent: r.UserAgent(), AcceptLanguage: r.Header.Get("Accept-Language"), r: r, header: w.Header(),
	})
}

func info(ctx context.Context) *reqInfo {
	if ri, ok := ctx.Value(reqInfoKey{}).(*reqInfo); ok {
		return ri
	}
	return &reqInfo{header: http.Header{}}
}

// setCookie 追加一个 Set-Cookie。生成的响应类型只能设置一个 Set-Cookie，
// 而登录与刷新要同时下发访问令牌与刷新令牌（AUTH-08）。
func setCookie(ctx context.Context, c *http.Cookie) {
	if v := c.String(); v != "" {
		info(ctx).header.Add("Set-Cookie", v)
	}
}

// cookie 返回请求中名为 name 的 Cookie 值。
func cookie(ctx context.Context, name string) string {
	ri := info(ctx)
	if ri.r == nil {
		return ""
	}
	c, err := ri.r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}
