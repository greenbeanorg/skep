package auth

import "testing"

func TestRoleAllows(t *testing.T) {
	admin := Role{ID: RoleAdmin, Permissions: []string{PermAll}}
	viewer := Role{ID: RoleViewer, Permissions: []string{PermSystemRead, PermNodeRead}}
	none := Role{ID: "empty"}

	for _, p := range Permissions {
		if !admin.Allows(p) {
			t.Errorf("admin is denied %s", p)
		}
		if none.Allows(p) {
			t.Errorf("a role without permissions is allowed %s", p)
		}
	}
	if !viewer.Allows(PermSystemRead) || viewer.Allows(PermUserManage) || viewer.Allows(PermTaskCancel) {
		t.Error("viewer permissions are wrong")
	}
	// Deny by default: what is not a declared permission is allowed to
	// nobody, the wildcard included.
	for _, p := range []string{"", "*", "vm:delete", "system:read ", "SYSTEM:READ"} {
		if admin.Allows(p) || viewer.Allows(p) {
			t.Errorf("undeclared permission %q was allowed", p)
		}
	}
	// A role that lists the wildcard next to other things is still all.
	if !(Role{Permissions: []string{PermNodeRead, PermAll}}).Allows(PermAuditRead) {
		t.Error("wildcard not honoured")
	}
	seen := map[string]bool{}
	for _, p := range Permissions {
		if seen[p] || !KnownPermission(p) {
			t.Errorf("permission list is inconsistent at %q", p)
		}
		seen[p] = true
	}
}
