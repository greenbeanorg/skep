package database

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/greenbeanorg/skep/migrations"
)

// TestStorageSchemaRules covers what agent/0005 adds (ADR 0010 decisions
// 2, 3, 7, 8, 10, 13).
func TestStorageSchemaRules(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db, migrations.Agent()); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO nodes (id, name, is_local) VALUES ('n1', 'node1', 1)`)
	mustExec(t, db, `INSERT INTO storage_pools (id, node_id, name, type) VALUES ('p1', 'n1', 'skeptest', 'zfs')`)
	mustExec(t, db, `INSERT INTO virtual_machines (id, node_id, name, engine, cpu_count, memory_bytes) VALUES ('v1', 'n1', 'vm1', 'bhyve', 1, 1073741824)`)

	// Snapshots name a resource that exists, with a valid label.
	mustExec(t, db, `INSERT INTO snapshots (id, resource_kind, resource_id, name, consistency) VALUES ('s1', 'vm', 'v1', 'before-upgrade', 'consistent')`)
	mustExec(t, db, `INSERT INTO snapshots (id, resource_kind, resource_id, name, consistency) VALUES ('s2', 'vm', 'v1', 'second.1', 'crash_consistent')`)
	for name, stmt := range map[string]string{
		"a missing VM":                   `INSERT INTO snapshots (id, resource_kind, resource_id, name, consistency) VALUES ('x', 'vm', 'nope', 'a', 'consistent')`,
		"a missing container":            `INSERT INTO snapshots (id, resource_kind, resource_id, name, consistency) VALUES ('x', 'container', 'v1', 'a', 'consistent')`,
		"an unknown kind":                `INSERT INTO snapshots (id, resource_kind, resource_id, name, consistency) VALUES ('x', 'disk', 'v1', 'a', 'consistent')`,
		"a duplicate label":              `INSERT INTO snapshots (id, resource_kind, resource_id, name, consistency) VALUES ('x', 'vm', 'v1', 'before-upgrade', 'consistent')`,
		"a label with a slash":           `INSERT INTO snapshots (id, resource_kind, resource_id, name, consistency) VALUES ('x', 'vm', 'v1', 'a/b', 'consistent')`,
		"a label with an @":              `INSERT INTO snapshots (id, resource_kind, resource_id, name, consistency) VALUES ('x', 'vm', 'v1', 'a@b', 'consistent')`,
		"a leading dash":                 `INSERT INTO snapshots (id, resource_kind, resource_id, name, consistency) VALUES ('x', 'vm', 'v1', '-a', 'consistent')`,
		"an empty label":                 `INSERT INTO snapshots (id, resource_kind, resource_id, name, consistency) VALUES ('x', 'vm', 'v1', '', 'consistent')`,
		"a 64-character label":           `INSERT INTO snapshots (id, resource_kind, resource_id, name, consistency) VALUES ('x', 'vm', 'v1', '` + string(make64()) + `', 'consistent')`,
		"an unknown kind of consistency": `INSERT INTO snapshots (id, resource_kind, resource_id, name, consistency) VALUES ('x', 'vm', 'v1', 'a', 'application_consistent')`,
		"moving a snapshot":              `UPDATE snapshots SET resource_id = 'v2' WHERE id = 's1'`,
	} {
		wantConstraintError(t, db, name, stmt)
	}

	// A linked clone protects its origin snapshot, and so its source VM.
	mustExec(t, db, `INSERT INTO virtual_machines (id, node_id, name, engine, cpu_count, memory_bytes, cloned_from_snapshot_id)
		VALUES ('v2', 'n1', 'clone', 'bhyve', 1, 1073741824, 's1')`)
	wantConstraintError(t, db, "delete the origin snapshot", `DELETE FROM snapshots WHERE id = 's1'`)
	wantConstraintError(t, db, "delete the source VM", `DELETE FROM virtual_machines WHERE id = 'v1'`)
	wantConstraintError(t, db, "a clone of a missing snapshot",
		`INSERT INTO virtual_machines (id, node_id, name, engine, cpu_count, memory_bytes, cloned_from_snapshot_id) VALUES ('v3', 'n1', 'c3', 'bhyve', 1, 1073741824, 'nope')`)
	// A snapshot nothing depends on can go.
	mustExec(t, db, `DELETE FROM snapshots WHERE id = 's2'`)
	// Once the clone is gone, deleting the source takes its snapshot rows.
	mustExec(t, db, `DELETE FROM virtual_machines WHERE id = 'v2'`)
	mustExec(t, db, `DELETE FROM virtual_machines WHERE id = 'v1'`)
	var left int
	if err := db.QueryRow(`SELECT count(*) FROM snapshots`).Scan(&left); err != nil || left != 0 {
		t.Errorf("snapshots left after the VM was deleted: %d, %v", left, err)
	}

	// Templates: a dist template carries a checksum, a pkgbase one does not.
	sha := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	mustExec(t, db, `INSERT INTO container_templates (id, node_id, storage_pool_id, name, source, packages) VALUES ('t1', 'n1', 'p1', 'base-15.1', 'pkgbase', '["FreeBSD-set-minimal"]')`)
	mustExec(t, db, `INSERT INTO container_templates (id, node_id, storage_pool_id, name, source, archive_sha256) VALUES ('t2', 'n1', 'p1', 'dist-15.1', 'dist', ?)`, sha)
	wantConstraintError(t, db, "dist without a checksum", `INSERT INTO container_templates (id, node_id, storage_pool_id, name, source) VALUES ('x', 'n1', 'p1', 'x', 'dist')`)
	wantConstraintError(t, db, "pkgbase with a checksum", `INSERT INTO container_templates (id, node_id, storage_pool_id, name, source, archive_sha256) VALUES ('x', 'n1', 'p1', 'x', 'pkgbase', ?)`, sha)
	wantConstraintError(t, db, "a checksum in upper case", `INSERT INTO container_templates (id, node_id, storage_pool_id, name, source, archive_sha256) VALUES ('x', 'n1', 'p1', 'x', 'dist', ?)`, "ABCDEF"+sha[6:])
	wantConstraintError(t, db, "an unknown source", `INSERT INTO container_templates (id, node_id, storage_pool_id, name, source) VALUES ('x', 'n1', 'p1', 'x', 'url')`)

	// Containers: started_once never goes back.
	mustExec(t, db, `INSERT INTO containers (id, node_id, name, template_id, storage_pool_id) VALUES ('c1', 'n1', 'jail1', 't1', 'p1')`)
	mustExec(t, db, `UPDATE containers SET started_once = 1 WHERE id = 'c1'`)
	wantConstraintError(t, db, "started_once back to 0", `UPDATE containers SET started_once = 0 WHERE id = 'c1'`)
	wantConstraintError(t, db, "delete a template in use", `DELETE FROM container_templates WHERE id = 't1'`)
	mustExec(t, db, `INSERT INTO snapshots (id, resource_kind, resource_id, name, consistency) VALUES ('s3', 'container', 'c1', 'a', 'consistent')`)

	// Container networks: addressing is consistent with its mode.
	mustExec(t, db, `INSERT INTO container_networks (id, container_id, slot, bridge, mac_address) VALUES ('cn0', 'c1', 0, 'bridge0', '02:00:00:00:01:01')`)
	mustExec(t, db, `INSERT INTO container_networks (id, container_id, slot, bridge, ipv4_mode, ipv4_address, ipv4_gateway, ipv6_mode)
		VALUES ('cn1', 'c1', 1, 'bridge0', 'static', '192.0.2.10/24', '192.0.2.1', 'slaac')`)
	for name, stmt := range map[string]string{
		"static without an address": `INSERT INTO container_networks (id, container_id, slot, bridge, ipv4_mode) VALUES ('x', 'c1', 2, 'bridge0', 'static')`,
		"dhcp with an address":      `INSERT INTO container_networks (id, container_id, slot, bridge, ipv4_address) VALUES ('x', 'c1', 2, 'bridge0', '192.0.2.5/24')`,
		"dhcp with a gateway":       `INSERT INTO container_networks (id, container_id, slot, bridge, ipv4_gateway) VALUES ('x', 'c1', 2, 'bridge0', '192.0.2.1')`,
		"slaac with an address":     `INSERT INTO container_networks (id, container_id, slot, bridge, ipv6_mode, ipv6_address) VALUES ('x', 'c1', 2, 'bridge0', 'slaac', '2001:db8::5/64')`,
		"a duplicate slot":          `INSERT INTO container_networks (id, container_id, slot, bridge) VALUES ('x', 'c1', 0, 'bridge0')`,
		"a duplicate MAC":           `INSERT INTO container_networks (id, container_id, slot, bridge, mac_address) VALUES ('x', 'c1', 2, 'bridge0', '02:00:00:00:01:01')`,
	} {
		wantConstraintError(t, db, name, stmt)
	}
	// Deleting the container takes its networks and its snapshot rows.
	mustExec(t, db, `DELETE FROM containers WHERE id = 'c1'`)
	var nets, snaps int
	_ = db.QueryRow(`SELECT count(*) FROM container_networks`).Scan(&nets)
	_ = db.QueryRow(`SELECT count(*) FROM snapshots`).Scan(&snaps)
	if nets != 0 || snaps != 0 {
		t.Errorf("after the container was deleted: %d networks, %d snapshots", nets, snaps)
	}
}

func make64() []byte {
	b := make([]byte, 64)
	for i := range b {
		b[i] = 'a'
	}
	return b
}

// TestStoragePermissionsMigration: api/0005 gives the storage and jail
// permissions to the built-in roles, storage:write to none of them, and
// leaves the roles protected.
func TestStoragePermissionsMigration(t *testing.T) {
	ctx := context.Background()
	all := migrations.API()
	known, err := LoadMigrations(all)
	if err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t)
	if _, err := Migrate(ctx, db, migrationPrefix(t, all, known, 4)); err != nil {
		t.Fatal(err)
	}
	applied, err := Migrate(ctx, db, migrationPrefix(t, all, known, 5))
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0].Name != "storage_permissions" {
		t.Fatalf("applied = %+v", applied)
	}
	perms := func(role string) map[string]bool {
		var raw string
		if err := db.QueryRow(`SELECT permissions FROM roles WHERE id = ?`, role).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var list []string
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			t.Fatal(err)
		}
		sort.Strings(list)
		out := map[string]bool{}
		for _, p := range list {
			out[p] = true
		}
		return out
	}
	operator, viewer, admin := perms("operator"), perms("viewer"), perms("admin")
	for _, p := range []string{"vm:snapshot", "container:read", "container:write", "container:delete", "container:power", "container:snapshot",
		// Kept from 0004 and before.
		"system:read", "node:read", "task:read", "task:cancel", "user:read", "role:read", "audit:read",
		"vm:read", "vm:write", "vm:delete", "vm:power", "vm:console", "storage:read"} {
		if !operator[p] {
			t.Errorf("operator lacks %s", p)
		}
	}
	if operator["storage:write"] || operator["user:manage"] || operator["*"] {
		t.Errorf("operator has too much: %v", operator)
	}
	for _, p := range []string{"container:read", "vm:read", "storage:read", "system:read", "node:read", "task:read", "role:read"} {
		if !viewer[p] {
			t.Errorf("viewer lacks %s", p)
		}
	}
	for _, p := range []string{"vm:snapshot", "container:write", "container:delete", "container:power", "container:snapshot", "storage:write"} {
		if viewer[p] {
			t.Errorf("viewer has %s", p)
		}
	}
	if len(admin) != 1 || !admin["*"] {
		t.Errorf("admin = %v", admin)
	}
	wantConstraintError(t, db, "update a built-in role", `UPDATE roles SET permissions = '["*"]' WHERE id = 'operator'`)
}
