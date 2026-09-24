// SPDX-License-Identifier: AGPL-3.0-or-later

// Command oapiprep 为 oapi-codegen 准备 panel-spec 的 OpenAPI 文档，并生成操作元数据。
//
// oapi-codegen 只支持 OpenAPI 3.0：3.1 的 `type: [X, 'null']` 会被当作可选字段并加 omitempty，
// 必填的可空字段因此在响应中缺失。本工具把它改写为 3.0 的 `type: X` 加 `nullable: true`，
// 配合 oapi-codegen 的 nullable-type 选项输出 null。契约文件本身不变（panel-spec 是字段级事实来源）。
//
// 同时从契约生成每个操作的元数据（路由、认证方式、是否接受 Idempotency-Key），
// 供 clientapi 的中间件使用，避免在代码中手工维护一份与契约重复的清单。
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
	metaSrc, err := renderMeta(pkg, ops)
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
			ops = append(ops, op{
				ID:         id.Value,
				Method:     strings.ToUpper(m),
				Path:       path,
				Auth:       authMode(get(o, "security"), global),
				Idempotent: hasIdempotencyKey(get(o, "parameters")) || hasIdempotencyKey(shared),
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

func hasIdempotencyKey(params *yaml.Node) bool {
	if params == nil {
		return false
	}
	for _, p := range params.Content {
		if ref := get(p, "$ref"); ref != nil && strings.HasSuffix(ref.Value, "/IdempotencyKey") {
			return true
		}
		if name := get(p, "name"); name != nil && strings.EqualFold(name.Value, "Idempotency-Key") {
			return true
		}
	}
	return false
}

func renderMeta(pkg string, ops []op) ([]byte, error) {
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
}

// Operations 按 ServeMux 模式索引全部操作（包括尚未实现的）。
var Operations = map[string]Operation{
`)
	for _, o := range ops {
		pattern := o.Method + " " + o.Path
		fmt.Fprintf(&b, "\t%q: {ID: %q, Method: %q, Pattern: %q, Auth: %s, Idempotent: %t},\n",
			pattern, o.ID, o.Method, pattern, o.Auth, o.Idempotent)
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}
