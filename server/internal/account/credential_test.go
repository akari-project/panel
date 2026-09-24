// SPDX-License-Identifier: AGPL-3.0-or-later

package account

import (
	"strings"
	"testing"
)

func TestRandomCode(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		c := randomCode(8)
		if len(c) != 8 || strings.ContainsAny(c, "01ILO") {
			t.Fatalf("bad code %q", c)
		}
		seen[c] = true
	}
	if len(seen) < 990 {
		t.Errorf("only %d distinct codes out of 1000", len(seen))
	}
}
