// SPDX-License-Identifier: AGPL-3.0-or-later

package export

import (
	"bytes"
	"testing"

	"github.com/google/uuid"

	"github.com/akari-project/panel/server/internal/specdata"
)

// EXP-09：各协议的凭据形式与 panel-spec testdata/node-v1-vectors.json 的 credential 向量一致。
func TestCredentialVectors(t *testing.T) {
	v := specdata.NodeV1Vectors(t).Credential
	secret, err := uuid.FromBytes(v.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if got := UUIDText(secret); got != v.UUIDText {
		t.Errorf("uuid_text = %s, want %s", got, v.UUIDText)
	}
	if got := SS2022Key16(secret); !bytes.Equal(got, v.SS2022Key16) {
		t.Errorf("ss2022_key_16 = %x, want %x", got, v.SS2022Key16)
	}
	if got := SS2022Key32(secret); !bytes.Equal(got, v.SS2022Key32) {
		t.Errorf("ss2022_key_32 = %x, want %x", got, v.SS2022Key32)
	}
}
