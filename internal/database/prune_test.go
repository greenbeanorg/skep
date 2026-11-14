package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
)

// addTask inserts a task in the given final status with n progress events.
// finishedAt is ignored for an unfinished task.
func addTask(t *testing.T, db *sql.DB, id, status, finishedAt string, resource [2]string, events int) {
	t.Helper()
	var rt, rid any
	if resource[0] != "" {
		rt, rid = resource[0], resource[1]
	}
	mustExec(t, db, insertTask, id, rt, rid)
	for range events {
		mustExec(t, db, `INSERT INTO task_events (task_id, message) VALUES (?, 'progress')`, id)
	}
	switch status {
	case "pending":
	case "running":
		mustExec(t, db, `UPDATE tasks SET status = 'running' WHERE id = ?`, id)
	default:
		mustExec(t, db, `UPDATE tasks SET status = 'running' WHERE id = ?`, id)
		mustExec(t, db, `UPDATE tasks SET status = ?, finished_at = ? WHERE id = ?`, status, finishedAt, id)
	}
}

func taskIDs(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT id FROM tasks ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return strings.Join(ids, " ")
}

func TestPruneFinished(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t)
	store := NewTaskStore(db)
	cutoff := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	const (
		old    = "2026-08-14T11:59:59.999Z" // one millisecond before the cutoff
		atEdge = "2026-08-14T12:00:00.000Z" // exactly the cutoff: not older
		older  = "2026-01-01T00:00:00.000Z"
	)
	none := [2]string{}
	vm1, vm2 := [2]string{"vm", "vm-1"}, [2]string{"vm", "vm-2"}

	// Old and finished, in every terminal status: these go.
	addTask(t, db, "gone-succeeded", "succeeded", old, none, 2)
	addTask(t, db, "gone-failed", "failed", older, none, 1)
	addTask(t, db, "gone-cancelled", "cancelled", older, none, 0)
	addTask(t, db, "gone-interrupted", "interrupted", older, none, 3)
	// Not older than the cutoff.
	addTask(t, db, "keep-at-edge", "succeeded", atEdge, none, 1)
	addTask(t, db, "keep-recent", "failed", "2026-11-01T00:00:00.000Z", none, 1)
	// Never finished, however long ago they were created.
	addTask(t, db, "keep-pending", "pending", "", none, 1)
	addTask(t, db, "keep-running", "running", "", none, 4)
	// vm-1: three old tasks. The newest finished one stays.
	addTask(t, db, "gone-vm1-a", "succeeded", older, vm1, 1)
	addTask(t, db, "gone-vm1-b", "failed", "2026-02-01T00:00:00.000Z", vm1, 1)
	addTask(t, db, "keep-vm1-newest", "succeeded", "2026-03-01T00:00:00.000Z", vm1, 5)
	// vm-2: its only finished task is old, and a newer one is still
	// running. The running one is not "the last operation": the old one
	// stays.
	addTask(t, db, "keep-vm2-only", "succeeded", older, vm2, 1)
	addTask(t, db, "keep-vm2-running", "running", "", vm2, 1)

	// In batches of two, as the retention task does in batches of 500.
	var tasksGone, eventsGone int64
	for {
		n, ev, err := store.PruneFinished(ctx, cutoff, 2)
		if err != nil {
			t.Fatal(err)
		}
		if n > 2 {
			t.Fatalf("a batch deleted %d tasks, more than its limit", n)
		}
		tasksGone, eventsGone = tasksGone+n, eventsGone+ev
		if n == 0 {
			break
		}
	}
	if tasksGone != 6 || eventsGone != 2+1+0+3+1+1 {
		t.Errorf("deleted %d tasks and %d events, want 6 and 8", tasksGone, eventsGone)
	}
	want := "keep-at-edge keep-pending keep-recent keep-running keep-vm1-newest keep-vm2-only keep-vm2-running"
	if got := taskIDs(t, db); got != want {
		t.Errorf("tasks left = %s\nwant         %s", got, want)
	}
	// Events went with their tasks, and only with them.
	var orphans, kept int
	if err := db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM task_events WHERE task_id NOT IN (SELECT id FROM tasks)),
		(SELECT COUNT(*) FROM task_events)`).Scan(&orphans, &kept); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 || kept != 1+1+1+4+5+1+1 {
		t.Errorf("%d orphaned events, %d events left; want 0 and 14", orphans, kept)
	}

	// A second run finds nothing.
	if n, ev, err := store.PruneFinished(ctx, cutoff, 500); err != nil || n != 0 || ev != 0 {
		t.Errorf("second run = %d, %d, %v", n, ev, err)
	}
	// When a newer task of vm-2 finishes, the old one loses its place.
	mustExec(t, db, `UPDATE tasks SET status = 'succeeded', finished_at = '2026-11-12T00:00:00.000Z' WHERE id = 'keep-vm2-running'`)
	if n, _, err := store.PruneFinished(ctx, cutoff, 500); err != nil || n != 1 {
		t.Errorf("after vm-2 finished a newer task: %d, %v; want 1", n, err)
	}
	if strings.Contains(taskIDs(t, db), "keep-vm2-only") {
		t.Error("the superseded vm-2 task is still there")
	}

	for _, bad := range []int{0, -1, maxPruneBatch + 1} {
		if _, _, err := store.PruneFinished(ctx, cutoff, bad); err == nil {
			t.Errorf("batch size %d was accepted", bad)
		}
	}
}

// TestIdempotencyKeyLivesAsLongAsItsTask checks what ADR 0007 documents:
// a replayed key returns the existing task while that task exists, and
// creates a new task once it has been deleted.
func TestIdempotencyKeyLivesAsLongAsItsTask(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t)
	store := NewTaskStore(db)
	created := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	request := func(id string, at time.Time) tasks.Task {
		return tasks.Task{
			ID: id, Type: "x.y", IdempotencyKey: "key-1", RequestHash: "hash-1",
			Actor: models.TaskActor{ID: "u1", Name: "andy"}, PeerUID: 1001,
			Payload: []byte(`{}`), CreatedAt: at,
		}
	}
	first, isNew, err := store.Create(ctx, request("task-1", created), "", nil)
	if err != nil || !isNew {
		t.Fatalf("Create = %+v, %v, %v", first, isNew, err)
	}
	for _, tr := range []tasks.Transition{
		{From: models.TaskPending, To: models.TaskRunning, At: created},
		{From: models.TaskRunning, To: models.TaskSucceeded, At: created.Add(time.Second)},
	} {
		if _, ok, err := store.Transition(ctx, "task-1", tr); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}

	// Before deletion: the replay is answered with the finished task.
	replay, isNew, err := store.Create(ctx, request("task-2", created.Add(time.Hour)), "", nil)
	if err != nil || isNew || replay.ID != "task-1" || replay.Status != models.TaskSucceeded {
		t.Fatalf("replay before deletion = %+v, new %v, %v", replay, isNew, err)
	}

	// Six days later the task is younger than the shortest retention
	// (7 days) and stays; the key still works.
	if n, _, err := store.PruneFinished(ctx, created.AddDate(0, 0, 6).AddDate(0, 0, -7), 500); err != nil || n != 0 {
		t.Fatalf("prune inside the retention time = %d, %v", n, err)
	}
	// Past the retention age it goes, and the key with it.
	if n, ev, err := store.PruneFinished(ctx, created.AddDate(0, 0, 91).AddDate(0, 0, -90), 500); err != nil || n != 1 || ev != 3 {
		t.Fatalf("prune = %d tasks, %d events, %v; want 1 and 3", n, ev, err)
	}
	if _, err := store.Get(ctx, "task-1"); err == nil {
		t.Fatal("the task is still readable")
	}
	if _, found, err := store.GetByKey(ctx, "key-1"); err != nil || found {
		t.Fatalf("the key still resolves: %v, %v", found, err)
	}

	// After deletion: the same key creates a second task.
	second, isNew, err := store.Create(ctx, request("task-3", created.AddDate(0, 0, 91)), "", nil)
	if err != nil || !isNew || second.ID != "task-3" {
		t.Fatalf("replay after deletion = %+v, new %v, %v", second, isNew, err)
	}

	// The audit log still says what the deleted task was and how it ended.
	rows, err := db.Query(`SELECT phase, outcome, COALESCE(task_status, '') FROM audit_log WHERE task_id = 'task-1' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var trail []string
	for rows.Next() {
		var phase, outcome, status string
		if err := rows.Scan(&phase, &outcome, &status); err != nil {
			t.Fatal(err)
		}
		trail = append(trail, fmt.Sprintf("%s/%s/%s", phase, outcome, status))
	}
	if got := strings.Join(trail, " "); got != "requested/success/ finished/success/succeeded" {
		t.Errorf("audit trail of the deleted task = %q", got)
	}
}

// TestFinishedAuditEntryCarriesErrorCodeAndDetails checks that the entry
// that outlives a task says how it ended: the error code, and whatever
// the task put on the record.
func TestFinishedAuditEntryCarriesErrorCodeAndDetails(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t)
	store := NewTaskStore(db)
	at := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	for id, tr := range map[string]tasks.Transition{
		"failed": {From: models.TaskRunning, To: models.TaskFailed, At: at, ErrorCode: "boom", ErrorMessage: "x",
			AuditDetails: map[string]any{"tasks_removed": 3, "error_code": "overridden"}},
		"succeeded": {From: models.TaskRunning, To: models.TaskSucceeded, At: at,
			AuditDetails: map[string]any{"tasks_removed": 7}},
		"plain": {From: models.TaskRunning, To: models.TaskSucceeded, At: at},
	} {
		if _, _, err := store.Create(ctx, tasks.Task{
			ID: id, Type: "x.y", RequestHash: "h", Actor: models.TaskActor{ID: "u1", Name: "andy"},
			Payload: []byte(`{}`), CreatedAt: at,
		}, "", nil); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := store.Transition(ctx, id, tasks.Transition{From: models.TaskPending, To: models.TaskRunning, At: at}); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if _, ok, err := store.Transition(ctx, id, tr); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	details := func(id string) string {
		var d sql.NullString
		if err := db.QueryRow(`SELECT details FROM audit_log WHERE task_id = ? AND phase = 'finished'`, id).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d.String
	}
	if got := details("failed"); got != `{"error_code":"boom","tasks_removed":3}` {
		t.Errorf("failed: details = %s", got)
	}
	if got := details("succeeded"); got != `{"tasks_removed":7}` {
		t.Errorf("succeeded: details = %s", got)
	}
	if got := details("plain"); got != "" {
		t.Errorf("plain: details = %s", got)
	}
}
