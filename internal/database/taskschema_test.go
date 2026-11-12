package database

import (
	"database/sql"
	"testing"
)

const insertTask = `INSERT INTO tasks (id, type, actor_id, actor_name, peer_uid, request_hash, resource_type, resource_id)
	VALUES (?, 'x.y', 'u1', 'andy', 1001, 'hash', ?, ?)`

func taskStatus(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var s string
	if err := db.QueryRow(`SELECT status FROM tasks WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestTaskSchemaRules checks that the database itself enforces the task
// rules of ADR 0005, independently of the engine.
func TestTaskSchemaRules(t *testing.T) {
	db := migratedDB(t)

	// A new task must be pending and carry peer_uid and request_hash.
	wantConstraintError(t, db, "task without peer_uid",
		`INSERT INTO tasks (id, type, actor_id, actor_name, request_hash) VALUES ('bad', 'x.y', 'u1', 'andy', 'h')`)
	wantConstraintError(t, db, "task without request_hash",
		`INSERT INTO tasks (id, type, actor_id, actor_name, peer_uid) VALUES ('bad', 'x.y', 'u1', 'andy', 0)`)
	wantConstraintError(t, db, "task created as running",
		`INSERT INTO tasks (id, type, status, actor_id, actor_name, peer_uid, request_hash) VALUES ('bad', 'x.y', 'running', 'u1', 'andy', 0, 'h')`)
	wantConstraintError(t, db, "unknown status",
		`INSERT INTO tasks (id, type, status, actor_id, actor_name, peer_uid, request_hash) VALUES ('bad', 'x.y', 'done', 'u1', 'andy', 0, 'h')`)

	// Every legal path.
	legal := map[string][]string{
		"a": {"running", "succeeded"},
		"b": {"running", "failed"},
		"c": {"running", "cancelled"},
		"d": {"running", "interrupted"},
		"e": {"cancelled"},
		"f": {"interrupted"},
	}
	for id, path := range legal {
		mustExec(t, db, insertTask, id, nil, nil)
		for _, to := range path {
			mustExec(t, db, `UPDATE tasks SET status = ? WHERE id = ?`, to, id)
		}
		if got := taskStatus(t, db, id); got != path[len(path)-1] {
			t.Errorf("task %s: status = %q", id, got)
		}
	}

	// Illegal transitions.
	mustExec(t, db, insertTask, "p", nil, nil)
	for _, to := range []string{"succeeded", "failed", "pending"} {
		wantConstraintError(t, db, "pending -> "+to, `UPDATE tasks SET status = ? WHERE id = 'p'`, to)
	}
	mustExec(t, db, `UPDATE tasks SET status = 'running' WHERE id = 'p'`)
	for _, to := range []string{"pending", "running"} {
		wantConstraintError(t, db, "running -> "+to, `UPDATE tasks SET status = ? WHERE id = 'p'`, to)
	}

	// A finished task is immutable: no state change, no edit of any column.
	for _, to := range []string{"pending", "running", "succeeded", "failed", "cancelled", "interrupted"} {
		wantConstraintError(t, db, "succeeded -> "+to, `UPDATE tasks SET status = ? WHERE id = 'a'`, to)
	}
	wantConstraintError(t, db, "edit result of a finished task", `UPDATE tasks SET result = '{}' WHERE id = 'a'`)
	wantConstraintError(t, db, "edit error of a finished task", `UPDATE tasks SET error = 'x' WHERE id = 'b'`)

	// The request fields never change, even while the task is active.
	for col, val := range map[string]any{
		"actor_id": "u2", "actor_name": "mallory", "peer_uid": 0, "type": "z.z",
		"payload": `{"a":1}`, "request_hash": "other", "idempotency_key": "k", "resource_id": "r",
	} {
		wantConstraintError(t, db, "edit "+col+" of a running task", `UPDATE tasks SET `+col+` = ? WHERE id = 'p'`, val)
	}
	// Bookkeeping columns can still be written while it runs.
	mustExec(t, db, `UPDATE tasks SET cancel_requested_at = '2026-11-12T00:00:00.000Z' WHERE id = 'p'`)

	// One unfinished task per resource.
	mustExec(t, db, insertTask, "r1", "vm", "vm-1")
	wantConstraintError(t, db, "second active task on a resource", insertTask, "r2", "vm", "vm-1")
	mustExec(t, db, insertTask, "r3", "vm", "vm-2") // a different resource is fine
	mustExec(t, db, `UPDATE tasks SET status = 'cancelled' WHERE id = 'r1'`)
	mustExec(t, db, insertTask, "r4", "vm", "vm-1") // free again once the first is finished

	// Events are append-only.
	mustExec(t, db, `INSERT INTO task_events (task_id, kind, message) VALUES ('p', 'state', 'task is running')`)
	wantConstraintError(t, db, "unknown event kind", `INSERT INTO task_events (task_id, kind, message) VALUES ('p', 'other', 'x')`)
	wantConstraintError(t, db, "event update", `UPDATE task_events SET message = 'edited'`)

	// Audit phase and task status are constrained.
	wantConstraintError(t, db, "unknown audit phase",
		`INSERT INTO audit_log (actor_id, actor_name, peer_uid, action, outcome, phase) VALUES ('u1', 'andy', 0, 'x.y', 'success', 'later')`)
	wantConstraintError(t, db, "non-terminal audit task status",
		`INSERT INTO audit_log (actor_id, actor_name, peer_uid, action, outcome, phase, task_status) VALUES ('u1', 'andy', 0, 'x.y', 'success', 'finished', 'running')`)
}
