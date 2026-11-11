package database

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/greenbeanorg/skep/migrations"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state", "skep.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func migratedDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openTestDB(t)
	if _, err := Migrate(context.Background(), db, migrations.SQLite()); err != nil {
		t.Fatal(err)
	}
	return db
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func TestOpenCreatesPrivateFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, "skep.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for p, want := range map[string]os.FileMode{dir: 0o700, path: 0o600} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s has mode %o, want %o", p, got, want)
		}
	}

	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys = %d, %v; want 1", fk, err)
	}
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, %v; want wal", mode, err)
	}
	if _, err := Open(context.Background(), ""); err == nil {
		t.Error("an empty path must be rejected")
	}
}

func TestMigrateCreatesSchema(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	applied, err := Migrate(ctx, db, migrations.SQLite())
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) == 0 || applied[0].Version != 1 || applied[0].Name != "initial_schema" {
		t.Fatalf("applied = %+v", applied)
	}

	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{
		"audit_log", "containers", "network_interfaces", "nodes", "roles",
		"schema_migrations", "storage_pools", "storage_volumes", "system_config",
		"task_events", "tasks", "users", "virtual_machines", "vm_disks", "vm_networks",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tables = %v\nwant     %v", got, want)
	}

	if v, err := SchemaVersion(ctx, db); err != nil || v != len(applied) {
		t.Errorf("SchemaVersion = %d, %v; want %d", v, err, len(applied))
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t)
	again, err := Migrate(ctx, db, migrations.SQLite())
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("second run applied %d migrations, want 0", len(again))
	}
}

func TestMigrateAppliesOnlyNewVersions(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	v1 := fstest.MapFS{"0001_first.sql": {Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY);`)}}
	v2 := fstest.MapFS{
		"0001_first.sql":  v1["0001_first.sql"],
		"0002_second.sql": {Data: []byte(`CREATE TABLE b (id INTEGER PRIMARY KEY); INSERT INTO b VALUES (7);`)},
	}
	if _, err := Migrate(ctx, db, v1); err != nil {
		t.Fatal(err)
	}
	applied, err := Migrate(ctx, db, v2)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0].Version != 2 {
		t.Fatalf("applied = %+v", applied)
	}
	var n int
	if err := db.QueryRow(`SELECT id FROM b`).Scan(&n); err != nil || n != 7 {
		t.Errorf("multi-statement migration not fully applied: %d, %v", n, err)
	}
}

func TestMigrateRollsBackFailedMigration(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	bad := fstest.MapFS{
		"0001_bad.sql": {Data: []byte(`CREATE TABLE half (id INTEGER PRIMARY KEY); CREATE TABLE half (id INTEGER);`)},
	}
	if _, err := Migrate(ctx, db, bad); err == nil {
		t.Fatal("a failing migration must return an error")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'half'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("failed migration left %d table(s) behind (err %v)", n, err)
	}
	if v, _ := SchemaVersion(ctx, db); v != 0 {
		t.Errorf("schema version = %d, want 0", v)
	}
}

func TestMigrateRejectsTamperingAndNewerDatabase(t *testing.T) {
	ctx := context.Background()
	orig := fstest.MapFS{
		"0001_first.sql":  {Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY);`)},
		"0002_second.sql": {Data: []byte(`CREATE TABLE b (id INTEGER PRIMARY KEY);`)},
	}

	db := openTestDB(t)
	if _, err := Migrate(ctx, db, orig); err != nil {
		t.Fatal(err)
	}
	edited := fstest.MapFS{
		"0001_first.sql":  {Data: []byte(`CREATE TABLE a (id INTEGER PRIMARY KEY, extra TEXT);`)},
		"0002_second.sql": orig["0002_second.sql"],
	}
	if _, err := Migrate(ctx, db, edited); err == nil || !strings.Contains(err.Error(), "changed after it was applied") {
		t.Errorf("edited migration: err = %v", err)
	}

	older := fstest.MapFS{"0001_first.sql": orig["0001_first.sql"]}
	if _, err := Migrate(ctx, db, older); err == nil || !strings.Contains(err.Error(), "newer schema") {
		t.Errorf("newer database: err = %v", err)
	}
}

func TestLoadMigrationsValidation(t *testing.T) {
	for name, fsys := range map[string]fstest.MapFS{
		"bad name": {"first.sql": {Data: []byte("SELECT 1;")}},
		"gap":      {"0001_a.sql": {Data: []byte("SELECT 1;")}, "0003_c.sql": {Data: []byte("SELECT 1;")}},
		"not at 1": {"0002_b.sql": {Data: []byte("SELECT 1;")}},
	} {
		if _, err := LoadMigrations(fsys); err == nil {
			t.Errorf("%s: LoadMigrations succeeded, want error", name)
		}
	}
}

// TestSchemaConstraints spot-checks the rules the schema must enforce.
func TestSchemaConstraints(t *testing.T) {
	db := migratedDB(t)
	mustExec(t, db, `INSERT INTO nodes (id, name, is_local) VALUES ('n1', 'node1', 1)`)

	mustFail := func(what, query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err == nil {
			t.Errorf("%s: statement succeeded, want a constraint error", what)
		}
	}

	mustFail("second local node", `INSERT INTO nodes (id, name, is_local) VALUES ('n2', 'node2', 1)`)
	mustFail("invalid task status", `INSERT INTO tasks (id, type, status) VALUES ('t0', 'x', 'done')`)
	mustFail("vm on unknown node",
		`INSERT INTO virtual_machines (id, node_id, name, engine, cpu_count, memory_bytes) VALUES ('v0', 'nope', 'vm0', 'bhyve', 1, 1024)`)
	mustFail("unknown hypervisor engine",
		`INSERT INTO virtual_machines (id, node_id, name, engine, cpu_count, memory_bytes) VALUES ('v0', 'n1', 'vm0', 'kvm', 1, 1024)`)
	mustFail("invalid json", `INSERT INTO tasks (id, type, payload) VALUES ('t0', 'x', '{not json')`)

	mustExec(t, db, `INSERT INTO roles (id, name) VALUES ('r1', 'admin')`)
	mustFail("plaintext password",
		`INSERT INTO users (id, username, password_hash, role_id) VALUES ('u0', 'root', 'hunter2', 'r1')`)
	mustExec(t, db,
		`INSERT INTO users (id, username, password_hash, role_id) VALUES ('u1', 'andy', '$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA', 'r1')`)

	for _, status := range []string{"pending", "running", "succeeded", "failed", "cancelled", "interrupted"} {
		mustExec(t, db, `INSERT INTO tasks (id, type, status) VALUES (?, 'x', ?)`, "task-"+status, status)
	}

	// Desired and observed VM state are independent columns.
	mustExec(t, db,
		`INSERT INTO virtual_machines (id, node_id, name, engine, cpu_count, memory_bytes, desired_state) VALUES ('v1', 'n1', 'vm1', 'bhyve', 2, 1073741824, 'running')`)
	var desired, observed string
	if err := db.QueryRow(`SELECT desired_state, observed_state FROM virtual_machines WHERE id = 'v1'`).Scan(&desired, &observed); err != nil {
		t.Fatal(err)
	}
	if desired != "running" || observed != "unknown" {
		t.Errorf("desired = %q, observed = %q", desired, observed)
	}

	// Deleting a VM removes its disks and NICs.
	mustExec(t, db, `INSERT INTO vm_disks (id, vm_id, slot, bus) VALUES ('d1', 'v1', 0, 'virtio-blk')`)
	mustExec(t, db, `INSERT INTO vm_networks (id, vm_id, slot, bridge) VALUES ('nic1', 'v1', 0, 'bridge0')`)
	mustExec(t, db, `DELETE FROM virtual_machines WHERE id = 'v1'`)
	var n int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM vm_disks) + (SELECT COUNT(*) FROM vm_networks)`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d disk/network rows left after VM delete (err %v)", n, err)
	}

	// The audit log is append-only.
	mustExec(t, db, `INSERT INTO audit_log (actor_name, action, outcome) VALUES ('system', 'agent.start', 'success')`)
	mustFail("audit update", `UPDATE audit_log SET action = 'edited'`)
	mustFail("audit delete", `DELETE FROM audit_log`)
}

func TestEnsureLocalNodeIsStable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "skep.db")

	open := func() *sql.DB {
		db, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Migrate(ctx, db, migrations.SQLite()); err != nil {
			t.Fatal(err)
		}
		return db
	}

	db := open()
	first, err := EnsureLocalNode(ctx, db, "merrick")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(first) {
		t.Errorf("node ID %q is not a version 4 UUID", first)
	}
	db.Close()

	// A restart, even with a different hostname, keeps the same identity.
	db = open()
	defer db.Close()
	second, err := EnsureLocalNode(ctx, db, "renamed-host")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Errorf("node ID changed across restarts: %q then %q", first, second)
	}
	var name string
	var count int
	if err := db.QueryRow(`SELECT MIN(name), COUNT(*) FROM nodes`).Scan(&name, &count); err != nil {
		t.Fatal(err)
	}
	if count != 1 || name != "merrick" {
		t.Errorf("nodes: count = %d, name = %q", count, name)
	}
}

func TestEnsureLocalNodeWithoutName(t *testing.T) {
	db := migratedDB(t)
	id, err := EnsureLocalNode(context.Background(), db, "")
	if err != nil {
		t.Fatal(err)
	}
	var name string
	if err := db.QueryRow(`SELECT name FROM nodes WHERE id = ?`, id).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "node-"+id[:8] {
		t.Errorf("name = %q", name)
	}
}
