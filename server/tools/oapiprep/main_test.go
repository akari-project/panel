// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const spec = `openapi: 3.1.0
info: {title: t, version: 1.0.0}
security:
  - bearer: []
  - cookie: []
paths:
  /v1/me:
    get:
      operationId: getMe
      responses: {'200': {description: OK}}
  /v1/accounts:
    post:
      operationId: createAccount
      security: []
      parameters:
        - $ref: '#/components/parameters/IdempotencyKey'
      responses: {'202': {description: OK}}
  /v1/plans:
    get:
      operationId: listPlans
      security:
        - {}
        - bearer: []
      responses: {'200': {description: OK}}
  /v1/sessions:
    post:
      operationId: createSession
      security: []
      responses: {'201': {description: OK}, '426': {description: Upgrade}}
components:
  schemas:
    Me:
      type: object
      required: [code]
      properties:
        code:
          type: [string, 'null']
        name:
          type: string
`

func TestRun(t *testing.T) {
	dir := t.TempDir()
	in, out, meta := filepath.Join(dir, "in.yaml"), filepath.Join(dir, "out.yaml"), filepath.Join(dir, "meta.go")
	if err := os.WriteFile(in, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(in, out, meta, "gen"); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	b, _ := os.ReadFile(out)
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["openapi"] != "3.0.3" {
		t.Errorf("openapi = %v", doc["openapi"])
	}
	code := doc["components"].(map[string]any)["schemas"].(map[string]any)["Me"].(map[string]any)["properties"].(map[string]any)["code"].(map[string]any)
	if code["type"] != "string" || code["nullable"] != true {
		t.Errorf("code = %v", code)
	}
	raw, _ := os.ReadFile(meta)
	m := strings.Join(strings.Fields(string(raw)), " ") // gofmt 会对齐 map 字面量
	for _, want := range []string{
		`"GET /v1/me": {ID: "getMe", Method: "GET", Pattern: "GET /v1/me", Auth: AuthRequired, Idempotent: false, VersionChecked: false}`,
		`"POST /v1/accounts": {ID: "createAccount", Method: "POST", Pattern: "POST /v1/accounts", Auth: AuthPublic, Idempotent: true, VersionChecked: false}`,
		`"GET /v1/plans": {ID: "listPlans", Method: "GET", Pattern: "GET /v1/plans", Auth: AuthOptional, Idempotent: false, VersionChecked: false}`,
		`"POST /v1/sessions": {ID: "createSession", Method: "POST", Pattern: "POST /v1/sessions", Auth: AuthPublic, Idempotent: false, VersionChecked: true}`,
		"// SPDX-License-Identifier: AGPL-3.0-or-later",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("meta missing %s\n%s", want, m)
		}
	}
}

func TestRejects(t *testing.T) {
	dir := t.TempDir()
	for name, s := range map[string]string{
		"3.0 input":       strings.Replace(spec, "3.1.0", "3.0.3", 1),
		"union type":      strings.Replace(spec, "[string, 'null']", "[string, integer]", 1),
		"no operationId":  strings.Replace(spec, "operationId: getMe\n", "", 1),
		"no top security": strings.Replace(spec, "security:\n  - bearer: []\n  - cookie: []\n", "", 1),
	} {
		in := filepath.Join(dir, "in.yaml")
		_ = os.WriteFile(in, []byte(s), 0o644)
		if err := run(in, filepath.Join(dir, "o.yaml"), filepath.Join(dir, "m.go"), "gen"); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}
