package auth

// Permissions are "resource:action" strings. This file is the only place
// they are declared. A milestone defines only the permissions it can
// enforce; later milestones add their own (for example vm:read, vm:power)
// together with a migration that gives them to the built-in roles.
const (
	PermSystemRead = "system:read"
	PermNodeRead   = "node:read"
	PermTaskRead   = "task:read"
	PermTaskCancel = "task:cancel"
	PermUserRead   = "user:read"
	PermUserManage = "user:manage"
	PermRoleRead   = "role:read"
	PermAuditRead  = "audit:read"

	// The first working VM (ADR 0009 decision 10). Deleting destroys
	// data and is its own permission.
	PermVMRead      = "vm:read"
	PermVMWrite     = "vm:write"
	PermVMDelete    = "vm:delete"
	PermVMPower     = "vm:power"
	PermVMConsole   = "vm:console"
	PermStorageRead = "storage:read"

	// Snapshots of VMs (ADR 0010 decision 14). A clone needs vm:write
	// as well.
	PermVMSnapshot = "vm:snapshot"
	// Building and deleting jail templates (ADR 0010 decision 14), for
	// admin only: it makes the root agent fetch and install packages.
	PermStorageWrite = "storage:write"

	// PermAll is how the admin role is stored: every permission,
	// including ones added later.
	PermAll = "*"
)

// Permissions lists every permission that exists.
var Permissions = []string{
	PermSystemRead, PermNodeRead, PermTaskRead, PermTaskCancel,
	PermUserRead, PermUserManage, PermRoleRead, PermAuditRead,
	PermVMRead, PermVMWrite, PermVMDelete, PermVMPower, PermVMConsole, PermStorageRead,
	PermVMSnapshot, PermStorageWrite,
}

// KnownPermission reports whether p is a declared permission.
func KnownPermission(p string) bool {
	for _, known := range Permissions {
		if p == known {
			return true
		}
	}
	return false
}

// Allows reports whether the role grants a permission. Anything that is
// not granted is denied: an empty or unknown permission is never allowed,
// not even for the admin role.
func (r Role) Allows(permission string) bool {
	if !KnownPermission(permission) {
		return false
	}
	for _, p := range r.Permissions {
		if p == PermAll || p == permission {
			return true
		}
	}
	return false
}

// ActionDenied is the audit action of a request refused for lack of a
// permission.
const ActionDenied = "authz.deny"
