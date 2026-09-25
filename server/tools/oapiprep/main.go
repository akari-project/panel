// SPDX-License-Identifier: AGPL-3.0-or-later

// Command oapiprep 为 oapi-codegen 准备 panel-spec 的 OpenAPI 文档，并生成操作元数据。
//
// oapi-codegen 只支持 OpenAPI 3.0：3.1 的 `type: [X, 'null']` 会被当作可选字段并加 omitempty，
// 必填的可空字段因此在响应中缺失。本工具把它改写为 3.0 的 `type: X` 加 `nullable: true`，
// 配合 oapi-codegen 的 nullable-type 选项输出 null。契约文件本身不变（panel-spec 是字段级事实来源）。
//
// 同时从契约生成每个操作的元数据（路由、认证方式、是否接受 Idempotency-Key；管理接口另有
// x-permission、x-sensitive 与是否要求 If-Match），以及管理接口的权限目录（components.schemas.Permission），
// 供 clientapi 与 consoleapi 的中间件使用，避免在代码中手工维护一份与契约重复的清单。
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

func main() {
	spec := flag.String("spec", "", "panel-spec 的 OpenAPI 文件")
	out := flag.String("out", "", "改写后的 OpenAPI 3.0 文件")
	meta := flag.String("meta", "", "操作元数据的 Go 文件")
	pkg := flag.String("pkg", "gen", "元数据文件的包名")
	flag.Parse()
	if *spec == "" || *out == "" || *meta == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*spec, *out, *meta, *pkg); err != nil {
		fmt.Fprintln(os.Stderr, "oapiprep:", err)
		os.Exit(1)
	}
}

func run(specPath, outPath, metaPath, pkg string) error {
	src, err := os.ReadFile(specPath)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return err
	}
	root := doc.Content[0]
	ops, err := operations(root)
	if err != nil {
		return err
	}
	catalog, err := permissionCatalog(root)
	if err != nil {
		return err
	}
	metaSrc, err := renderMeta(pkg, ops, catalog)
	if err != nil {
		return err
	}
	if err := downgrade(root); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := os.WriteFile(outPath, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.WriteFile(metaPath, metaSrc, 0o644)
}

func get(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// downgrade 把文档改写为 oapi-codegen 可以正确处理的 3.0 形式。
func downgrade(root *yaml.Node) error {
	v := get(root, "openapi")
	if v == nil || !strings.HasPrefix(v.Value, "3.1") {
		return fmt.Errorf("want an OpenAPI 3.1 document, got %v", v)
	}
	v.Value = "3.0.3"
	return walk(root)
}

func walk(n *yaml.Node) error {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Value == "type" && v.Kind == yaml.SequenceNode {
				var types []string
				nullable := false
				for _, t := range v.Content {
					if t.Value == "null" {
						nullable = true
						continue
					}
					types = append(types, t.Value)
				}
				if len(types) != 1 || !nullable {
					return fmt.Errorf("line %d: unsupported type list %v", v.Line, types)
				}
				n.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: types[0]}
				n.Content = append(n.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "nullable"},
					&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
				continue
			}
			if err := walk(v); err != nil {
				return err
			}
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, c := range n.Content {
			if err := walk(c); err != nil {
				return err
			}
		}
	}
	return nil
}

// Auth 取值与生成代码中的常量一致。
const (
	authPublic   = "AuthPublic"
	authOptional = "AuthOptional"
	authRequired = "AuthRequired"
)

type op struct {
	ID, Method, Path, Auth string
	Idempotent             bool
	// VersionChecked：响应中列出 426，即检查客户端最低版本的入口操作（spec/30 API-03）。
	VersionChecked bool
	// Permission 为 x-permission（管理接口，spec/10 AUTH-17），客户端接口为空。
	Permission string
	// Sensitive 为 x-sensitive（spec/10 AUTH-19）。
	Sensitive bool
	// IfMatch：参数中有 If-Match，即必须携带 If-Match 的操作（CONV-28）。
	IfMatch bool
}

var methods = []string{"get", "put", "post", "patch", "delete"}

func operations(root *yaml.Node) ([]op, error) {
	global := authMode(get(root, "security"), "")
	if global == "" {
		return nil, fmt.Errorf("missing top-level security")
	}
	paths := get(root, "paths")
	if paths == nil {
		return nil, fmt.Errorf("missing paths")
	}
	var ops []op
	for i := 0; i+1 < len(paths.Content); i += 2 {
		path, item := paths.Content[i].Value, paths.Content[i+1]
		shared := get(item, "parameters")
		for _, m := range methods {
			o := get(item, m)
			if o == nil {
				continue
			}
			id := get(o, "operationId")
			if id == nil {
				return nil, fmt.Errorf("%s %s: missing operationId", m, path)
			}
			perm := ""
			if p := get(o, "x-permission"); p != nil {
				if p.Kind != yaml.ScalarNode || p.Value == "" {
					return nil, fmt.Errorf("%s %s: x-permission must be a non-empty string", m, path)
				}
				perm = p.Value
			}
			ops = append(ops, op{
				ID:             id.Value,
				Method:         strings.ToUpper(m),
				Path:           path,
				Auth:           authMode(get(o, "security"), global),
				Idempotent:     hasParam(get(o, "parameters"), "IdempotencyKey", "Idempotency-Key") || hasParam(shared, "IdempotencyKey", "Idempotency-Key"),
				VersionChecked: get(get(o, "responses"), "426") != nil,
				Permission:     perm,
				Sensitive:      isTrue(get(o, "x-sensitive")),
				IfMatch:        hasParam(get(o, "parameters"), "IfMatch", "If-Match") || hasParam(shared, "IfMatch", "If-Match"),
			})
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].ID < ops[j].ID })
	return ops, nil
}

// authMode：空数组为无需认证；含空要求 {} 为可选认证；其余为必须认证。
func authMode(sec *yaml.Node, def string) string {
	if sec == nil {
		return def
	}
	if len(sec.Content) == 0 {
		return authPublic
	}
	for _, req := range sec.Content {
		if req.Kind == yaml.MappingNode && len(req.Content) == 0 {
			return authOptional
		}
	}
	return authRequired
}

// hasParam 报告参数列表中是否有以 components/parameters/<ref> 引用、或名为 name 的参数。
func hasParam(params *yaml.Node, ref, name string) bool {
	if params == nil {
		return false
	}
	for _, p := range params.Content {
		if r := get(p, "$ref"); r != nil && strings.HasSuffix(r.Value, "/"+ref) {
			return true
		}
		if n := get(p, "name"); n != nil && strings.EqualFold(n.Value, name) {
			return true
		}
	}
	return false
}

func isTrue(n *yaml.Node) bool { return n != nil && n.Kind == yaml.ScalarNode && n.Value == "true" }

// permissionCatalog 返回 components.schemas.Permission 的枚举（管理接口的权限目录，AUTH-17）；没有该模式时为空。
func permissionCatalog(root *yaml.Node) ([]string, error) {
	p := get(get(get(root, "components"), "schemas"), "Permission")
	if p == nil {
		return nil, nil
	}
	enum := get(p, "enum")
	if enum == nil || enum.Kind != yaml.SequenceNode || len(enum.Content) == 0 {
		return nil, fmt.Errorf("components.schemas.Permission: want a string enum")
	}
	var out []string
	for _, v := range enum.Content {
		out = append(out, v.Value)
	}
	return out, nil
}

func renderMeta(pkg string, ops []op, catalog []string) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// SPDX-License-Identifier: AGPL-3.0-or-later\n")
	fmt.Fprintf(&b, "// Code generated by tools/oapiprep from panel-spec. DO NOT EDIT.\n\n")
	fmt.Fprintf(&b, "package %s\n\n", pkg)
	b.WriteString(`// Auth 是操作的认证方式，取自契约的 security。
type Auth int

// 认证方式。
const (
	// AuthRequired：必须认证。
	AuthRequired Auth = iota
	// AuthOptional：可选认证，认证后返回与本人相关的内容。
	AuthOptional
	// AuthPublic：无需认证（security: []）。
	AuthPublic
)

// Operation 是一个操作的元数据。
type Operation struct {
	ID     string
	Method string
	// Pattern 为 net/http ServeMux 的模式，与生成的路由一致，同时用作日志中的路由模板（CONV-23）。
	Pattern string
	Auth    Auth
	// Idempotent 表示接受 Idempotency-Key（CONV-12）。
	Idempotent bool
	// VersionChecked 表示契约为该操作列出 426：自研客户端版本低于最低版本时拒绝（spec/30 API-03）。
	VersionChecked bool
	// Permission 为管理接口的 x-permission（spec/10 AUTH-17）：权限目录中的值，或 none、superadmin；客户端接口为空。
	Permission string
	// Sensitive 为 x-sensitive：敏感操作，要求原因与 Mfa-Assertion（spec/10 AUTH-19）。
	Sensitive bool
	// IfMatch 表示必须携带 If-Match（CONV-28）。
	IfMatch bool
}

// Operations 按 ServeMux 模式索引全部操作（包括尚未实现的）。
var Operations = map[string]Operation{
`)
	for _, o := range ops {
		pattern := o.Method + " " + o.Path
		fmt.Fprintf(&b, "\t%q: {ID: %q, Method: %q, Pattern: %q, Auth: %s, Idempotent: %t, VersionChecked: %t, Permission: %q, Sensitive: %t, IfMatch: %t},\n",
			pattern, o.ID, o.Method, pattern, o.Auth, o.Idempotent, o.VersionChecked, o.Permission, o.Sensitive, o.IfMatch)
	}
	b.WriteString("}\n")
	if len(catalog) > 0 {
		b.WriteString("\n// PermissionCatalog 是权限目录（components.schemas.Permission，spec/10 AUTH-17），按契约中的顺序。\nvar PermissionCatalog = []string{\n")
		for _, p := range catalog {
			fmt.Fprintf(&b, "\t%q,\n", p)
		}
		b.WriteString("}\n")
	}
	return format.Source(b.Bytes())
}
