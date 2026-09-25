// SPDX-License-Identifier: AGPL-3.0-or-later

package rbac

import (
	"testing"
)

func TestAllows(t *testing.T) {
	super := Staff{Roles: []string{"superadmin"}, Permissions: []string{"*"}}
	op := Staff{Roles: []string{"operator"}, Permissions: []string{"accounts.read", "plans.*", "settings.read"}}
	custom := Staff{Roles: []string{"billing"}, Permissions: []string{"*"}} // 即使权限被直接写成 *，也不是 superadmin 角色
	for _, c := range []struct {
		s        Staff
		required string
		want     bool
	}{
		{super, "none", true}, {super, "superadmin", true}, {super, "audit.read", true}, {super, "staff.*", true},
		{op, "none", true}, {op, "superadmin", false}, {op, "plans.*", true}, {op, "accounts.read", true},
		{op, "accounts.adjust", false}, {op, "settings.write", false}, {op, "", false},
		{custom, "superadmin", false},
	} {
		if got := c.s.Allows(c.required); got != c.want {
			t.Errorf("%v.Allows(%q) = %v", c.s.Roles, c.required, got)
		}
	}
}

func TestGrants(t *testing.T) {
	for _, c := range []struct {
		g, required string
		want        bool
	}{
		{"*", "plans.*", true}, {"plans.*", "plans.*", true}, {"accounts.*", "accounts.read", true},
		{"plans.*", "plans-x.read", false}, {"accounts.read", "accounts.adjust", false}, {"location-groups.*", "location.read", false},
	} {
		if got := Grants(c.g, c.required); got != c.want {
			t.Errorf("Grants(%q, %q) = %v", c.g, c.required, got)
		}
	}
}

func TestCheckCustomPermissions(t *testing.T) {
	catalog := []string{"accounts.read", "orders.read", "staff.*", "*"}
	for _, c := range []struct {
		perms []string
		code  string
	}{
		{[]string{"accounts.read", "orders.read"}, ""},
		{nil, "required"},
		{[]string{"nodes.write"}, "invalid_format"},
		{[]string{"accounts.read", "accounts.read"}, "invalid_format"},
		{[]string{"*"}, "not_allowed"},
		{[]string{"accounts.read", "staff.*"}, "not_allowed"},
	} {
		err := CheckCustomPermissions(c.perms, catalog)
		got := ""
		if err != nil {
			got = err.Fields[0].Code
		}
		if got != c.code {
			t.Errorf("%v: got %q, want %q", c.perms, got, c.code)
		}
	}
}
