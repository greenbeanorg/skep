package database

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"testing"
	"testing/fstest"

	"github.com/greenbeanorg/skep/migrations"
)

// TestVMSchemaRules covers what agent/0004 adds to the tables sketched in
// 0001 (ADR 0009 decision 2b).
func TestVMSchemaRules(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db, migrations.Agent()); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO nodes (id, name, is_local) VALUES ('n1', 'node1', 1)`)
	mustExec(t, db, `INSERT INTO storage_pools (id, node_id, name, type) VALUES ('p1', 'n1', 'skeptest', 'zfs')`)
	mustExec(t, db, `INSERT INTO storage_volumes (id, pool_id, name, kind, desired_size_bytes, desired_thin, desired_volblocksize)
		VALUES ('vol1', 'p1', 'skep/vm/v1/disk0', 'zvol', 8589934592, 1, 16384)`)
	mustExec(t, db, `INSERT INTO virtual_machines (id, node_id, name, engine, cpu_count, memory_bytes) VALUES ('v1', 'n1', 'vm1', 'bhyve', 2, 1073741824)`)

	// A new VM is not provisioned and has no process.
	var provisioned int
	var pid any
	if err := db.QueryRow(`SELECT provisioned, observed_pid FROM virtual_machines WHERE id = 'v1'`).Scan(&provisioned, &pid); err != nil {
		t.Fatal(err)
	}
	if provisioned != 0 || pid != nil {
		t.Errorf("provisioned = %d, observed_pid = %v", provisioned, pid)
	}
	wantConstraintError(t, db, "provisioned is a flag", `UPDATE virtual_machines SET provisioned = 2 WHERE id = 'v1'`)
	wantConstraintError(t, db, "a pid is positive", `UPDATE virtual_machines SET observed_pid = 0 WHERE id = 'v1'`)
	wantConstraintError(t, db, "thin is a flag", `UPDATE storage_volumes SET desired_thin = 2 WHERE id = 'vol1'`)

	// A disk has exactly one backing.
	mustExec(t, db, `INSERT INTO vm_disks (id, vm_id, storage_volume_id, slot, bus) VALUES ('d0', 'v1', 'vol1', 0, 'virtio-blk')`)
	mustExec(t, db, `INSERT INTO vm_disks (id, vm_id, slot, bus, read_only, media_name) VALUES ('d1', 'v1', 1, 'ahci-cd', 1, 'install.iso')`)
	for name, stmt := range map[string]string{
		"a disk without a backing":   `INSERT INTO vm_disks (id, vm_id, slot, bus) VALUES ('x', 'v1', 2, 'virtio-blk')`,
		"media on a disk bus":        `INSERT INTO vm_disks (id, vm_id, slot, bus, read_only, media_name) VALUES ('x', 'v1', 2, 'nvme', 1, 'install.iso')`,
		"a volume on the cd bus":     `INSERT INTO vm_disks (id, vm_id, storage_volume_id, slot, bus, read_only) VALUES ('x', 'v1', 'vol1', 2, 'ahci-cd', 1)`,
		"a cd without media":         `INSERT INTO vm_disks (id, vm_id, slot, bus, read_only) VALUES ('x', 'v1', 2, 'ahci-cd', 1)`,
		"both backings":              `INSERT INTO vm_disks (id, vm_id, storage_volume_id, slot, bus, read_only, media_name) VALUES ('x', 'v1', 'vol1', 2, 'ahci-cd', 1, 'install.iso')`,
		"writable media":             `INSERT INTO vm_disks (id, vm_id, slot, bus, read_only, media_name) VALUES ('x', 'v1', 2, 'ahci-cd', 0, 'install.iso')`,
		"a path as a media name":     `INSERT INTO vm_disks (id, vm_id, slot, bus, read_only, media_name) VALUES ('x', 'v1', 2, 'ahci-cd', 1, '../etc/passwd')`,
		"an empty media name":        `INSERT INTO vm_disks (id, vm_id, slot, bus, read_only, media_name) VALUES ('x', 'v1', 2, 'ahci-cd', 1, '')`,
		"update removes the backing": `UPDATE vm_disks SET storage_volume_id = NULL WHERE id = 'd0'`,
		"update adds media to disk":  `UPDATE vm_disks SET media_name = 'install.iso' WHERE id = 'd0'`,
		"update makes media a disk":  `UPDATE vm_disks SET bus = 'virtio-blk' WHERE id = 'd1'`,
	} {
		wantConstraintError(t, db, name, stmt)
	}
	// Swapping the medium is an update of the name alone.
	mustExec(t, db, `UPDATE vm_disks SET media_name = 'other.iso' WHERE id = 'd1'`)

	// A volume in use by a disk cannot be removed from under it.
	wantConstraintError(t, db, "delete a volume in use", `DELETE FROM storage_volumes WHERE id = 'vol1'`)

	// No VLAN on a NIC in this milestone.
	mustExec(t, db, `INSERT INTO vm_networks (id, vm_id, slot, bridge, mac_address) VALUES ('nic0', 'v1', 0, 'bridge0', '02:00:00:00:00:01')`)
	wantConstraintError(t, db, "vlan on insert", `INSERT INTO vm_networks (id, vm_id, slot, bridge, vlan_tag) VALUES ('x', 'v1', 1, 'bridge0', 60)`)
	wantConstraintError(t, db, "vlan on update", `UPDATE vm_networks SET vlan_tag = 60 WHERE id = 'nic0'`)
	wantConstraintError(t, db, "duplicate MAC", `INSERT INTO vm_networks (id, vm_id, slot, bridge, mac_address) VALUES ('x', 'v1', 1, 'bridge0', '02:00:00:00:00:01')`)
	mustExec(t, db, `UPDATE vm_networks SET observed_tap = 'tap0' WHERE id = 'nic0'`)
	mustExec(t, db, `UPDATE storage_pools SET observed_available_bytes = 1, observed_error = NULL WHERE id = 'p1'`)
}

// TestVMPermissionsMigration: api/0004 gives the six permissions to the
// built-in roles and leaves them as protected as they were.
func TestVMPermissionsMigration(t *testing.T) {
	ctx := context.Background()
	all := migrations.API()
	known, err := LoadMigrations(all)
	if err != nil {
		t.Fatal(err)
	}
	// Upgrade a database that is at 0003, as an installed one was, to
	// 0004 alone.
	db := openTestDB(t)
	if _, err := Migrate(ctx, db, migrationPrefix(t, all, known, 3)); err != nil {
		t.Fatal(err)
	}
	applied, err := Migrate(ctx, db, migrationPrefix(t, all, known, 4))
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0].Name != "vm_permissions" {
		t.Fatalf("applied = %+v", applied)
	}

	perms := func(role string) []string {
		var raw string
		if err := db.QueryRow(`SELECT permissions FROM roles WHERE id = ?`, role).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var list []string
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			t.Fatal(err)
		}
		sort.Strings(list)
		return list
	}
	has := func(list []string, p string) bool {
		i := sort.SearchStrings(list, p)
		return i < len(list) && list[i] == p
	}
	operator, viewer, admin := perms("operator"), perms("viewer"), perms("admin")
	for _, p := range []string{"vm:read", "vm:write", "vm:delete", "vm:power", "vm:console", "storage:read"} {
		if !has(operator, p) {
			t.Errorf("operator lacks %s", p)
		}
	}
	for _, p := range []string{"vm:read", "storage:read"} {
		if !has(viewer, p) {
			t.Errorf("viewer lacks %s", p)
		}
	}
	for _, p := range []string{"vm:write", "vm:delete", "vm:power", "vm:console", "task:cancel", "user:read"} {
		if has(viewer, p) {
			t.Errorf("viewer has %s", p)
		}
	}
	// What the roles had before is still there.
	for _, p := range []string{"system:read", "node:read", "task:read", "task:cancel", "user:read", "role:read", "audit:read"} {
		if !has(operator, p) {
			t.Errorf("operator lost %s", p)
		}
	}
	if has(operator, "user:manage") || has(operator, "*") || len(admin) != 1 || admin[0] != "*" {
		t.Errorf("operator = %v, admin = %v", operator, admin)
	}
	// The protection is back.
	wantConstraintError(t, db, "update a built-in role", `UPDATE roles SET permissions = '["*"]' WHERE id = 'viewer'`)
	wantConstraintError(t, db, "delete a built-in role", `DELETE FROM roles WHERE id = 'viewer'`)
}

// migrationPrefix is the first n migrations of a set: a database as an
// older release left it.
func migrationPrefix(t *testing.T, all fs.FS, known []Migration, n int) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	for _, m := range known[:n] {
		name := fmt.Sprintf("%04d_%s.sql", m.Version, m.Name)
		body, err := fs.ReadFile(all, name)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = &fstest.MapFile{Data: body}
	}
	return out
}
