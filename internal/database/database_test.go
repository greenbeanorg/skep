package database

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"

	"github.com/greenbeanorg/skep/migrations"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state", "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// migratedDB returns a database with the agent schema applied.
func migratedDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openTestDB(t)
	if _, err := Migrate(context.Background(), db, migrations.Agent()); err != nil {
		t.Fatal(err)
	}
	return db
}

// migratedAPIDB returns a database with the API schema applied.
func migratedAPIDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openTestDB(t)
	if _, err := Migrate(context.Background(), db, migrations.API()); err != nil {
		t.Fatal(err)
	}
	return db
}

func tableNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func TestOpenCreatesPrivateFiles(t *testing.T) {
	// Layout used in production: <data dir>/agent/agent.db.
	dataDir := filepath.Join(t.TempDir(), "data")
	dir := filepath.Join(dataDir, "agent")
	path := filepath.Join(dir, "agent.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mode := func(p string) os.FileMode {
		t.Helper()
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode().Perm()
	}
	// The directory holding the database and the file itself are private.
	if got := mode(dir); got != 0o700 {
		t.Errorf("%s has mode %o, want 700", dir, got)
	}
	if got := mode(path); got != 0o600 {
		t.Errorf("%s has mode %o, want 600", path, got)
	}
	// The data directory above it is shared by the agent and the API, which
	// run as different users, so it must not be created owner-only.
	umask := syscall.Umask(0)
	syscall.Umask(umask)
	if got, want := mode(dataDir), os.FileMode(0o755&^umask); got != want {
		t.Errorf("%s has mode %o, want %o", dataDir, got, want)
	}

	// An existing directory is left as it is.
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	db2, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	db2.Close()
	if got := mode(dir); got != 0o750 {
		t.Errorf("Open changed an existing directory to mode %o", got)
	}

	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys = %d, %v; want 1", fk, err)
	}
	var journal string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil || journal != "wal" {
		t.Errorf("journal_mode = %q, %v; want wal", journal, err)
	}
	if _, err := Open(context.Background(), ""); err == nil {
		t.Error("an empty path must be rejected")
	}
}

func TestMigrateCreatesAgentSchema(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	applied, err := Migrate(ctx, db, migrations.Agent())
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) == 0 || applied[0].Version != 1 || applied[0].Name != "initial_schema" {
		t.Fatalf("applied = %+v", applied)
	}
	got := tableNames(t, db)
	want := []string{
		"audit_log", "containers", "network_interfaces", "nodes", "schema_migrations",
		"storage_pools", "storage_volumes", "system_config", "task_events", "tasks",
		"virtual_machines", "vm_disks", "vm_networks",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tables = %v\nwant     %v", got, want)
	}
	if v, err := SchemaVersion(ctx, db); err != nil || v != len(applied) {
		t.Errorf("SchemaVersion = %d, %v; want %d", v, err, len(applied))
	}
}

func TestMigrateCreatesAPISchema(t *testing.T) {
	got := tableNames(t, migratedAPIDB(t))
	want := []string{"audit_log", "roles", "schema_migrations", "users"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tables = %v\nwant     %v", got, want)
	}
}

// TestDatabasesDoNotOverlap guards the privilege boundary in ADR 0004: the
// agent's database holds no identity data and the API's database holds no
// node state. Each side keeps its own audit log.
func TestDatabasesDoNotOverlap(t *testing.T) {
	agent := tableNames(t, migratedDB(t))
	api := tableNames(t, migratedAPIDB(t))
	shared := map[string]bool{"schema_migrations": true, "audit_log": true}

	inAPI := map[string]bool{}
	for _, name := range api {
		inAPI[name] = true
	}
	for _, name := range agent {
		if inAPI[name] && !shared[name] {
			t.Errorf("table %q exists in both agent.db and api.db", name)
		}
		if name == "users" || name == "roles" || name == "sessions" {
			t.Errorf("identity table %q must not be in agent.db", name)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t)
	again, err := Migrate(ctx, db, migrations.Agent())
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

func wantConstraintError(t *testing.T, db *sql.DB, what, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err == nil {
		t.Errorf("%s: statement succeeded, want a constraint error", what)
	}
}

// TestAgentSchemaConstraints spot-checks the rules agent.db must enforce.
func TestAgentSchemaConstraints(t *testing.T) {
	db := migratedDB(t)
	mustExec(t, db, `INSERT INTO nodes (id, name, is_local) VALUES ('n1', 'node1', 1)`)

	wantConstraintError(t, db, "second local node", `INSERT INTO nodes (id, name, is_local) VALUES ('n2', 'node2', 1)`)
	wantConstraintError(t, db, "vm on unknown node",
		`INSERT INTO virtual_machines (id, node_id, name, engine, cpu_count, memory_bytes) VALUES ('v0', 'nope', 'vm0', 'bhyve', 1, 1024)`)
	wantConstraintError(t, db, "unknown hypervisor engine",
		`INSERT INTO virtual_machines (id, node_id, name, engine, cpu_count, memory_bytes) VALUES ('v0', 'n1', 'vm0', 'kvm', 1, 1024)`)

	// Tasks: every documented status is accepted, and the asserted actor is
	// mandatory.
	for _, status := range []string{"pending", "running", "succeeded", "failed", "cancelled", "interrupted"} {
		mustExec(t, db, `INSERT INTO tasks (id, type, status, actor_id, actor_name) VALUES (?, 'x', ?, 'u1', 'andy')`, "task-"+status, status)
	}
	wantConstraintError(t, db, "invalid task status",
		`INSERT INTO tasks (id, type, status, actor_id, actor_name) VALUES ('t0', 'x', 'done', 'u1', 'andy')`)
	wantConstraintError(t, db, "invalid json",
		`INSERT INTO tasks (id, type, payload, actor_id, actor_name) VALUES ('t0', 'x', '{not json', 'u1', 'andy')`)
	wantConstraintError(t, db, "task without actor", `INSERT INTO tasks (id, type) VALUES ('t0', 'x')`)
	wantConstraintError(t, db, "task with empty actor",
		`INSERT INTO tasks (id, type, actor_id, actor_name) VALUES ('t0', 'x', '', '')`)

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

	// The operation audit needs both the asserted actor and the verified
	// peer UID, and is append-only.
	mustExec(t, db,
		`INSERT INTO audit_log (actor_id, actor_name, peer_uid, action, outcome) VALUES ('u1', 'andy', 1001, 'vm.start', 'success')`)
	wantConstraintError(t, db, "audit without actor",
		`INSERT INTO audit_log (peer_uid, action, outcome) VALUES (1001, 'vm.start', 'success')`)
	wantConstraintError(t, db, "audit with empty actor",
		`INSERT INTO audit_log (actor_id, actor_name, peer_uid, action, outcome) VALUES ('', '', 1001, 'vm.start', 'success')`)
	wantConstraintError(t, db, "audit without peer uid",
		`INSERT INTO audit_log (actor_id, actor_name, action, outcome) VALUES ('u1', 'andy', 'vm.start', 'success')`)
	wantConstraintError(t, db, "audit update", `UPDATE audit_log SET action = 'edited'`)
	wantConstraintError(t, db, "audit delete", `DELETE FROM audit_log`)
}

// TestAPISchemaConstraints spot-checks the rules api.db must enforce.
func TestAPISchemaConstraints(t *testing.T) {
	db := migratedAPIDB(t)

	mustExec(t, db, `INSERT INTO roles (id, name) VALUES ('r1', 'admin')`)
	wantConstraintError(t, db, "plaintext password",
		`INSERT INTO users (id, username, password_hash, role_id) VALUES ('u0', 'root', 'hunter2', 'r1')`)
	wantConstraintError(t, db, "user with unknown role",
		`INSERT INTO users (id, username, password_hash, role_id) VALUES ('u0', 'root', '$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA', 'nope')`)
	mustExec(t, db,
		`INSERT INTO users (id, username, password_hash, role_id) VALUES ('u1', 'andy', '$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA', 'r1')`)
	wantConstraintError(t, db, "deleting a role that is in use", `DELETE FROM roles WHERE id = 'r1'`)

	// A failed login for an unknown name has no actor ID.
	mustExec(t, db, `INSERT INTO audit_log (actor_name, action, outcome) VALUES ('mallory', 'auth.login', 'denied')`)
	wantConstraintError(t, db, "audit update", `UPDATE audit_log SET action = 'edited'`)
	wantConstraintError(t, db, "audit delete", `DELETE FROM audit_log`)
}

func TestEnsureLocalNodeIsStable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")

	open := func() *sql.DB {
		db, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Migrate(ctx, db, migrations.Agent()); err != nil {
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
