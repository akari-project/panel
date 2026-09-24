// SPDX-License-Identifier: AGPL-3.0-or-later

package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactsSensitiveKeys(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(&buf, "debug", "json")
	if err != nil {
		t.Fatal(err)
	}
	log.Info("x", "password", "hunter2", "refresh_token", "abc", "Enroll_Token_Hash", "h", KeyRoute, "GET /healthz")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"password", "refresh_token", "Enroll_Token_Hash"} {
		if rec[k] != Redacted {
			t.Errorf("%s = %v, want redacted", k, rec[k])
		}
	}
	if rec[KeyRoute] != "GET /healthz" {
		t.Errorf("route = %v", rec[KeyRoute])
	}
	if strings.Contains(buf.String(), "hunter2") {
		t.Error("secret leaked into log output")
	}
}

func TestNewRejectsUnknown(t *testing.T) {
	if _, err := New(&bytes.Buffer{}, "loud", "json"); err == nil {
		t.Error("want error for unknown level")
	}
	if _, err := New(&bytes.Buffer{}, "info", "xml"); err == nil {
		t.Error("want error for unknown format")
	}
}
