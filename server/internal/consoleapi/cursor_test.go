// SPDX-License-Identifier: AGPL-3.0-or-later

package consoleapi

import (
	"encoding/base64"
	"testing"

	"github.com/google/uuid"
)

// 游标必须恰好是一个 JSON 对象（CONV-11）：null、数组、多余内容、未知字段与非 base64url 都返回 400 invalid_format。
func TestDecodeCursor(t *testing.T) {
	enc := func(s string) *string {
		v := base64.RawURLEncoding.EncodeToString([]byte(s))
		return &v
	}
	id := uuid.NewString()
	var cur struct {
		ID uuid.UUID `json:"id"`
	}
	if ok, err := decodeCursor(enc(`{"id":"`+id+`"}`), &cur); !ok || err != nil || cur.ID.String() != id {
		t.Fatalf("valid cursor: %v %v %v", ok, err, cur.ID)
	}
	if ok, err := decodeCursor(nil, &cur); ok || err != nil {
		t.Fatalf("no cursor: %v %v", ok, err)
	}
	bad := "%%"
	for name, c := range map[string]*string{
		"null":          enc(`null`),
		"array":         enc(`[1]`),
		"trailing":      enc(`{"id":"` + id + `"}{}`),
		"trailing junk": enc(`{"id":"` + id + `"} x`),
		"unknown field": enc(`{"id":"` + id + `","x":1}`),
		"empty":         enc(`  `),
		"not base64url": &bad,
		"wrong type":    enc(`{"id":1}`),
	} {
		if ok, err := decodeCursor(c, &cur); ok || err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
