package tasks_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/migrations"
	"github.com/greenbeanorg/skep/pkg/models"
)

// The engine is tested against the real SQLite store, so these tests also
// cover the transactions and the database rules.

var (
	andy    = models.TaskActor{ID: "u-andy", Name: "andy"}
	mallory = models.TaskActor{ID: "u-mallory", Name: "mallory"}
)

const peerUID = 1001

type fixture struct {
	t      *testing.T
	db     *sql.DB
	engine *tasks.Engine
	// release unblocks handlers of type "test.block".
	release chan struct{}
	// started receives the ID of each task whose handler began.
	started chan string
	runs    atomic.Int64
}

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := database.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Migrate(context.Background(), db, migrations.Agent()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// localNode returns the ID of the database's local node, creating it.
func localNode(t *testing.T, db *sql.DB) string {
	t.Helper()
	id, err := database.EnsureLocalNode(context.Background(), db, "testnode")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// newFixture builds an engine on db with the test task types and starts it.
func newFixture(t *testing.T, db *sql.DB, workers, queue int) *fixture {
	t.Helper()
	f := &fixture{t: t, db: db, release: make(chan struct{}), started: make(chan string, 64)}
	e, err := tasks.New(tasks.Options{
		Store:     database.NewTaskStore(db),
		Audit:     database.NewAuditLog(db),
		NodeID:    localNode(t, db),
		Workers:   workers,
		QueueSize: queue,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.engine = e

	// block waits until released, or returns ctx.Err() when cancelled.
	block := func(ctx context.Context, task tasks.Task, r tasks.Reporter) (any, error) {
		f.runs.Add(1)
		r.Progress("waiting", map[string]string{"step": "1"})
		// Signal only after the progress event is stored: a test that has
		// seen "started" may then rely on the handler having nothing left
		// to write until it is released or cancelled.
		f.started <- task.ID
		select {
		case <-f.release:
			return map[string]string{"done": "yes"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	defs := []tasks.Definition{
		{Type: "test.ok", Cancellable: true, Timeout: time.Minute,
			Run: func(context.Context, tasks.Task, tasks.Reporter) (any, error) {
				f.runs.Add(1)
				return map[string]int{"answer": 42}, nil
			}},
		{Type: "test.block", Cancellable: true, Timeout: time.Minute, Run: block},
		{Type: "test.block_uncancellable", Cancellable: false, Timeout: time.Minute, Run: block},
		// stubborn ignores cancellation and finishes anyway.
		{Type: "test.stubborn", Cancellable: true, Timeout: time.Minute,
			Run: func(_ context.Context, task tasks.Task, _ tasks.Reporter) (any, error) {
				f.started <- task.ID
				<-f.release
				return "finished anyway", nil
			}},
		{Type: "test.fail", Timeout: time.Minute,
			Run: func(context.Context, tasks.Task, tasks.Reporter) (any, error) {
				return nil, &tasks.Failure{Code: "pool_missing", Message: "the pool does not exist"}
			}},
		{Type: "test.error", Timeout: time.Minute,
			Run: func(context.Context, tasks.Task, tasks.Reporter) (any, error) {
				return nil, errors.New("open /var/db/skep/secret-path: permission denied")
			}},
		{Type: "test.panic", Timeout: time.Minute,
			Run: func(context.Context, tasks.Task, tasks.Reporter) (any, error) { panic("boom") }},
		{Type: "test.slow", Cancellable: true, Timeout: 30 * time.Millisecond,
			Run: func(ctx context.Context, _ tasks.Task, _ tasks.Reporter) (any, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}},
		{Type: "test.validated", Timeout: time.Minute,
			Validate: func(p json.RawMessage) error {
				var v struct {
					Size int `json:"size"`
				}
				if err := json.Unmarshal(p, &v); err != nil || v.Size <= 0 {
					return errors.New("size must be positive")
				}
				return nil
			},
			Run: func(context.Context, tasks.Task, tasks.Reporter) (any, error) { return nil, nil }},
	}
	for _, d := range defs {
		if err := e.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.Shutdown(ctx)
	})
	return f
}

func newDefaultFixture(t *testing.T) *fixture {
	return newFixture(t, openDB(t, filepath.Join(t.TempDir(), "agent.db")), 4, 16)
}

func (f *fixture) submit(req tasks.Request) tasks.Task {
	f.t.Helper()
	if req.Actor.ID == "" {
		req.Actor = andy
	}
	req.PeerUID = peerUID
	task, created, err := f.engine.Submit(context.Background(), req)
	if err != nil {
		f.t.Fatalf("submit %s: %v", req.Type, err)
	}
	if !created {
		f.t.Fatalf("submit %s: returned an existing task", req.Type)
	}
	return task
}

// wait polls until the task reaches a terminal state or want.
func (f *fixture) wait(id string, want models.TaskStatus) tasks.Task {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		task, err := f.engine.Get(context.Background(), id)
		if err != nil {
			f.t.Fatal(err)
		}
		if task.Status == want {
			return task
		}
		if task.Status.Terminal() || time.Now().After(deadline) {
			f.t.Fatalf("task %s is %q (error %q), want %q", id, task.Status, task.ErrorCode, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (f *fixture) waitStarted() string {
	f.t.Helper()
	select {
	case id := <-f.started:
		return id
	case <-time.After(10 * time.Second):
		f.t.Fatal("no handler started")
		return ""
	}
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
	return n
}

type auditRow struct {
	actorID, actorName, action, phase, outcome, taskStatus, taskID string
	peerUID                                                        int
}

func (f *fixture) auditRows(where string, args ...any) []auditRow {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT actor_id, actor_name, peer_uid, action, phase, outcome,
		COALESCE(task_status, ''), COALESCE(task_id, '') FROM audit_log WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.actorID, &r.actorName, &r.peerUID, &r.action, &r.phase, &r.outcome, &r.taskStatus, &r.taskID); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func wantCode(t *testing.T, err error, code string) *models.Error {
	t.Helper()
	var me *models.Error
	if !errors.As(err, &me) || me.Code != code {
		t.Fatalf("error = %v, want code %q", err, code)
	}
	return me
}

func eventSummary(t *testing.T, f *fixture, id string) string {
	t.Helper()
	events, err := f.engine.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, e := range events {
		parts = append(parts, e.Kind+":"+e.Message)
	}
	return strings.Join(parts, " | ")
}

func TestTaskRunsToSucceeded(t *testing.T) {
	f := newDefaultFixture(t)
	task := f.submit(tasks.Request{Type: "test.block", ResourceType: "vm", ResourceID: "vm-1", RequestID: "req-1"})
	if task.Status != models.TaskPending || task.NodeID != localNode(t, f.db) || task.PeerUID != peerUID {
		t.Errorf("new task = %+v", task)
	}
	f.waitStarted()
	running := f.wait(task.ID, models.TaskRunning)
	if running.StartedAt == nil || running.FinishedAt != nil {
		t.Errorf("running task timestamps: %+v", running)
	}
	close(f.release)

	done := f.wait(task.ID, models.TaskSucceeded)
	if string(done.Result) != `{"done":"yes"}` || done.ErrorCode != "" || done.FinishedAt == nil {
		t.Errorf("finished task = %+v", done)
	}
	if got, want := eventSummary(t, f, task.ID),
		"state:task is pending | state:task is running | progress:waiting | state:task is succeeded"; got != want {
		t.Errorf("events = %s\nwant     %s", got, want)
	}

	// One "requested" and one "finished" audit entry, both naming the actor
	// and the verified peer UID.
	rows := f.auditRows(`task_id = ?`, task.ID)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %+v", rows)
	}
	want := []auditRow{
		{andy.ID, andy.Name, "test.block", "requested", "success", "", task.ID, peerUID},
		{andy.ID, andy.Name, "test.block", "finished", "success", "succeeded", task.ID, peerUID},
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("audit row %d = %+v\nwant          %+v", i, rows[i], want[i])
		}
	}

	pub := done.Public()
	if pub.Actor != andy || pub.Error != nil || pub.Result == nil || pub.Status != models.TaskSucceeded {
		t.Errorf("public task = %+v", pub)
	}
}

func TestSubmitWithoutActorWritesNothing(t *testing.T) {
	f := newDefaultFixture(t)
	for _, actor := range []models.TaskActor{{}, {ID: "u1"}, {Name: "andy"}} {
		_, _, err := f.engine.Submit(context.Background(), tasks.Request{Type: "test.ok", Actor: actor, PeerUID: peerUID})
		if !errors.Is(err, audit.ErrActorRequired) {
			t.Errorf("actor %+v: error = %v, want ErrActorRequired", actor, err)
		}
	}
	if n := f.count(`SELECT (SELECT COUNT(*) FROM tasks) + (SELECT COUNT(*) FROM audit_log)`); n != 0 {
		t.Errorf("%d rows were written for requests without an actor", n)
	}
	if f.runs.Load() != 0 {
		t.Error("a task without an actor ran")
	}
}

func TestInvalidSubmissionsAreRefusedAndAudited(t *testing.T) {
	f := newDefaultFixture(t)
	bad := map[string]tasks.Request{
		"unknown type":       {Type: "test.nope"},
		"malformed type":     {Type: "DROP TABLE tasks"},
		"half a resource":    {Type: "test.ok", ResourceType: "vm"},
		"bad resource id":    {Type: "test.ok", ResourceType: "vm", ResourceID: "../../etc/passwd"},
		"bad key":            {Type: "test.ok", IdempotencyKey: "has space"},
		"payload not json":   {Type: "test.ok", Payload: json.RawMessage(`{`)},
		"payload not object": {Type: "test.ok", Payload: json.RawMessage(`[1]`)},
		"payload rejected":   {Type: "test.validated", Payload: json.RawMessage(`{"size":0}`)},
	}
	for name, req := range bad {
		req.Actor, req.PeerUID = andy, peerUID
		_, _, err := f.engine.Submit(context.Background(), req)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		wantCode(t, err, models.CodeInvalidRequest)
	}
	if n := f.count(`SELECT COUNT(*) FROM tasks`); n != 0 {
		t.Errorf("%d tasks were created from invalid requests", n)
	}
	rows := f.auditRows(`1 = 1`)
	if len(rows) != len(bad) {
		t.Fatalf("%d audit rows for %d refusals", len(rows), len(bad))
	}
	for _, r := range rows {
		if r.phase != "requested" || r.outcome != "failure" || r.taskID != "" || r.actorID != andy.ID || r.peerUID != peerUID {
			t.Errorf("audit row = %+v", r)
		}
		if strings.Contains(r.action, " ") {
			t.Errorf("an arbitrary string was written as the audit action: %q", r.action)
		}
	}

	// A valid payload for the same type is accepted.
	ok := f.submit(tasks.Request{Type: "test.validated", Payload: json.RawMessage(`{"size": 5}`)})
	f.wait(ok.ID, models.TaskSucceeded)
}

func TestFailureModes(t *testing.T) {
	f := newDefaultFixture(t)
	tests := []struct {
		taskType, code, message string
	}{
		{"test.fail", "pool_missing", "the pool does not exist"},
		{"test.error", tasks.CodeFailed, "the task failed; see the agent log"},
		{"test.panic", tasks.CodePanic, "the task failed with an internal error"},
		{"test.slow", tasks.CodeTimeout, "the task exceeded its time limit"},
	}
	for _, tt := range tests {
		task := f.wait(f.submit(tasks.Request{Type: tt.taskType}).ID, models.TaskFailed)
		if task.ErrorCode != tt.code || task.ErrorMessage != tt.message {
			t.Errorf("%s: error = %q / %q, want %q / %q", tt.taskType, task.ErrorCode, task.ErrorMessage, tt.code, tt.message)
		}
		if len(task.Result) != 0 || task.FinishedAt == nil {
			t.Errorf("%s: %+v", tt.taskType, task)
		}
		if strings.Contains(task.ErrorMessage, "secret-path") {
			t.Errorf("%s: internal error text was stored on the task", tt.taskType)
		}
		rows := f.auditRows(`task_id = ? AND phase = 'finished'`, task.ID)
		if len(rows) != 1 || rows[0].outcome != "failure" || rows[0].taskStatus != "failed" {
			t.Errorf("%s: finished audit = %+v", tt.taskType, rows)
		}
	}
}

func TestBusyResourceIsRefusedNotQueued(t *testing.T) {
	f := newDefaultFixture(t)
	first := f.submit(tasks.Request{Type: "test.block", ResourceType: "vm", ResourceID: "vm-1"})
	f.waitStarted()

	_, _, err := f.engine.Submit(context.Background(), tasks.Request{
		Type: "test.ok", ResourceType: "vm", ResourceID: "vm-1", Actor: mallory, PeerUID: peerUID,
	})
	me := wantCode(t, err, models.CodeResourceBusy)
	if me.Details["task_id"] != first.ID {
		t.Errorf("busy error details = %v, want the blocking task's ID", me.Details)
	}
	if n := f.count(`SELECT COUNT(*) FROM tasks`); n != 1 {
		t.Errorf("%d tasks exist; the second must not be queued", n)
	}
	denied := f.auditRows(`outcome = 'denied'`)
	if len(denied) != 1 || denied[0].actorID != mallory.ID || denied[0].action != "test.ok" || denied[0].taskID != "" {
		t.Errorf("denied audit = %+v", denied)
	}

	// A different resource is not affected, and the first resource is free
	// again once its task has finished.
	other := f.submit(tasks.Request{Type: "test.ok", ResourceType: "vm", ResourceID: "vm-2"})
	f.wait(other.ID, models.TaskSucceeded)
	close(f.release)
	f.wait(first.ID, models.TaskSucceeded)
	again := f.submit(tasks.Request{Type: "test.ok", ResourceType: "vm", ResourceID: "vm-1"})
	f.wait(again.ID, models.TaskSucceeded)
}

func TestIdempotencyKeys(t *testing.T) {
	f := newDefaultFixture(t)
	ctx := context.Background()
	req := tasks.Request{
		Type: "test.fail", IdempotencyKey: "key-1", Actor: andy, PeerUID: peerUID,
		Payload: json.RawMessage(`{"b": 2, "a": 1}`),
	}
	first, created, err := f.engine.Submit(ctx, req)
	if err != nil || !created {
		t.Fatalf("first submit: created=%v err=%v", created, err)
	}

	// Same key, same request, with the payload keys in another order.
	replay := req
	replay.Payload = json.RawMessage(`{"a":1,"b":2}`)
	again, created, err := f.engine.Submit(ctx, replay)
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("replay: id=%s created=%v err=%v", again.ID, created, err)
	}

	// The key keeps answering after the task has finished, and a failed
	// task is returned as failed, not retried.
	f.wait(first.ID, models.TaskFailed)
	done, created, err := f.engine.Submit(ctx, req)
	if err != nil || created || done.ID != first.ID || done.Status != models.TaskFailed {
		t.Fatalf("replay after finish: %+v created=%v err=%v", done, created, err)
	}

	// Same key, different request.
	conflicts := map[string]func(*tasks.Request){
		"different payload":  func(r *tasks.Request) { r.Payload = json.RawMessage(`{"a":1,"b":3}`) },
		"different type":     func(r *tasks.Request) { r.Type = "test.ok" },
		"different resource": func(r *tasks.Request) { r.ResourceType, r.ResourceID = "vm", "vm-9" },
		"different actor":    func(r *tasks.Request) { r.Actor = mallory },
	}
	for name, change := range conflicts {
		r := req
		change(&r)
		if _, _, err := f.engine.Submit(ctx, r); err == nil {
			t.Errorf("%s: accepted", name)
		} else {
			wantCode(t, err, models.CodeIdempotencyConflict)
		}
	}

	if n := f.count(`SELECT COUNT(*) FROM tasks`); n != 1 {
		t.Errorf("%d tasks exist, want 1", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM audit_log WHERE phase = 'requested' AND outcome = 'success'`); n != 1 {
		t.Errorf("%d accepted-request audit entries, want 1: replays must not add entries", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM audit_log WHERE outcome = 'denied'`); n != len(conflicts) {
		t.Errorf("%d denied audit entries, want %d", n, len(conflicts))
	}
}

func TestConcurrentSubmitsWithOneKeyCreateOneTask(t *testing.T) {
	f := newDefaultFixture(t)
	const n = 24
	var (
		wg      sync.WaitGroup
		created atomic.Int64
		mu      sync.Mutex
		idsSeen = map[string]bool{}
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task, isNew, err := f.engine.Submit(context.Background(), tasks.Request{
				Type: "test.ok", IdempotencyKey: "same-key", Actor: andy, PeerUID: peerUID,
			})
			if err != nil {
				t.Errorf("submit: %v", err)
				return
			}
			if isNew {
				created.Add(1)
			}
			mu.Lock()
			idsSeen[task.ID] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if created.Load() != 1 || len(idsSeen) != 1 {
		t.Errorf("%d submissions created a task and %d distinct IDs were returned; want 1 and 1", created.Load(), len(idsSeen))
	}
}

func TestCancelPendingTask(t *testing.T) {
	f := newFixture(t, openDB(t, filepath.Join(t.TempDir(), "agent.db")), 1, 16)
	blocker := f.submit(tasks.Request{Type: "test.block"})
	f.waitStarted()
	// The only worker is busy, so this one stays pending. Even a task of a
	// non-cancellable type can be cancelled before it starts.
	pending := f.submit(tasks.Request{Type: "test.block_uncancellable"})

	got, err := f.engine.Cancel(context.Background(), pending.ID, tasks.CancelRequest{Actor: mallory, PeerUID: 0})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.TaskCancelled || got.ErrorCode != tasks.CodeCancelled || got.StartedAt != nil || got.FinishedAt == nil {
		t.Errorf("cancelled task = %+v", got)
	}

	close(f.release)
	f.wait(blocker.ID, models.TaskSucceeded)
	if f.runs.Load() != 1 {
		t.Errorf("%d handlers ran; the cancelled task must never start", f.runs.Load())
	}

	rows := f.auditRows(`task_id = ?`, pending.ID)
	want := []auditRow{
		{andy.ID, andy.Name, "test.block_uncancellable", "requested", "success", "", pending.ID, peerUID},
		// The finished entry names the submitter; the cancel entry names
		// the actor who cancelled.
		{andy.ID, andy.Name, "test.block_uncancellable", "finished", "failure", "cancelled", pending.ID, peerUID},
		{mallory.ID, mallory.Name, "task.cancel", "requested", "success", "", pending.ID, 0},
	}
	if len(rows) != len(want) {
		t.Fatalf("audit rows = %+v", rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("audit row %d = %+v\nwant          %+v", i, rows[i], want[i])
		}
	}
}

func TestCancelRunningTask(t *testing.T) {
	f := newDefaultFixture(t)
	task := f.submit(tasks.Request{Type: "test.block"})
	f.waitStarted()
	f.wait(task.ID, models.TaskRunning)

	got, err := f.engine.Cancel(context.Background(), task.ID, tasks.CancelRequest{Actor: mallory, PeerUID: peerUID})
	if err != nil {
		t.Fatal(err)
	}
	if got.CancelRequestedAt == nil {
		t.Errorf("cancel request was not recorded: %+v", got)
	}
	done := f.wait(task.ID, models.TaskCancelled)
	if done.ErrorCode != tasks.CodeCancelled || done.CancelRequestedAt == nil {
		t.Errorf("cancelled task = %+v", done)
	}
	if !strings.Contains(eventSummary(t, f, task.ID), "state:cancel requested | state:task is cancelled") {
		t.Errorf("events = %s", eventSummary(t, f, task.ID))
	}
	if rows := f.auditRows(`action = 'task.cancel'`); len(rows) != 1 || rows[0].actorID != mallory.ID {
		t.Errorf("cancel audit = %+v", rows)
	}

	// Cancelling again, now that it is finished, changes nothing.
	again, err := f.engine.Cancel(context.Background(), task.ID, tasks.CancelRequest{Actor: mallory, PeerUID: peerUID})
	if err != nil || again.Status != models.TaskCancelled || !again.FinishedAt.Equal(*done.FinishedAt) {
		t.Errorf("second cancel: %+v, %v", again, err)
	}
	if n := f.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'task.cancel'`); n != 1 {
		t.Errorf("%d cancel audit entries, want 1", n)
	}
}

func TestCancelRunningNonCancellableTaskIsRefused(t *testing.T) {
	f := newDefaultFixture(t)
	task := f.submit(tasks.Request{Type: "test.block_uncancellable"})
	f.waitStarted()
	f.wait(task.ID, models.TaskRunning)

	_, err := f.engine.Cancel(context.Background(), task.ID, tasks.CancelRequest{Actor: mallory, PeerUID: peerUID})
	wantCode(t, err, models.CodeNotCancellable)

	still, _ := f.engine.Get(context.Background(), task.ID)
	if still.Status != models.TaskRunning || still.CancelRequestedAt != nil {
		t.Errorf("a refused cancel changed the task: %+v", still)
	}
	if rows := f.auditRows(`action = 'task.cancel'`); len(rows) != 1 || rows[0].outcome != "denied" {
		t.Errorf("cancel audit = %+v", rows)
	}
	close(f.release)
	f.wait(task.ID, models.TaskSucceeded)
}

func TestCancelThatLosesTheRaceKeepsTheRealOutcome(t *testing.T) {
	f := newDefaultFixture(t)
	task := f.submit(tasks.Request{Type: "test.stubborn"})
	f.waitStarted()
	f.wait(task.ID, models.TaskRunning)

	if _, err := f.engine.Cancel(context.Background(), task.ID, tasks.CancelRequest{Actor: andy, PeerUID: peerUID}); err != nil {
		t.Fatal(err)
	}
	close(f.release) // the handler ignored the cancel and completes
	done := f.wait(task.ID, models.TaskSucceeded)
	if done.CancelRequestedAt == nil || string(done.Result) != `"finished anyway"` {
		t.Errorf("task = %+v", done)
	}
}

func TestCancelErrors(t *testing.T) {
	f := newDefaultFixture(t)
	ctx := context.Background()
	if _, err := f.engine.Cancel(ctx, "no-such-task", tasks.CancelRequest{Actor: andy}); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("unknown task: %v", err)
	}
	task := f.submit(tasks.Request{Type: "test.ok"})
	f.wait(task.ID, models.TaskSucceeded)
	if _, err := f.engine.Cancel(ctx, task.ID, tasks.CancelRequest{}); !errors.Is(err, audit.ErrActorRequired) {
		t.Errorf("cancel without actor: %v", err)
	}
}

// TestRestartInterruptsUnfinishedTasks simulates a crash: an engine with a
// running and a pending task is abandoned, and a new engine starts on the
// same database.
func TestRestartInterruptsUnfinishedTasks(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "agent.db"))

	crashed := newFixture(t, db, 1, 16)
	running := crashed.submit(tasks.Request{Type: "test.block", ResourceType: "vm", ResourceID: "vm-1"})
	crashed.waitStarted()
	crashed.wait(running.ID, models.TaskRunning)
	pending := crashed.submit(tasks.Request{Type: "test.ok", ResourceType: "vm", ResourceID: "vm-2"})
	finished := crashed.submit(tasks.Request{Type: "test.ok", Actor: mallory})
	_ = finished // stays pending behind the blocker as well

	restarted := newFixture(t, db, 2, 16)

	for _, id := range []string{running.ID, pending.ID, finished.ID} {
		task, err := restarted.engine.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status != models.TaskInterrupted || task.ErrorCode != tasks.CodeAgentRestart || task.FinishedAt == nil {
			t.Errorf("task %s after restart = %+v", id, task)
		}
	}
	// Give the new engine time to (wrongly) pick anything up.
	time.Sleep(50 * time.Millisecond)
	if restarted.runs.Load() != 0 {
		t.Errorf("the restarted engine ran %d old tasks; unfinished work must never be resumed", restarted.runs.Load())
	}

	// Each interrupted task has a finished audit entry carrying the
	// identity of whoever submitted it.
	rows := restarted.auditRows(`phase = 'finished'`)
	if len(rows) != 3 {
		t.Fatalf("finished audit rows = %+v", rows)
	}
	actors := map[string]string{}
	for _, r := range rows {
		if r.outcome != "failure" || r.taskStatus != "interrupted" || r.peerUID != peerUID {
			t.Errorf("audit row = %+v", r)
		}
		actors[r.taskID] = r.actorID
	}
	if actors[running.ID] != andy.ID || actors[finished.ID] != mallory.ID {
		t.Errorf("audit actors = %v", actors)
	}

	// The resources are free again, and new work runs normally.
	fresh := restarted.submit(tasks.Request{Type: "test.ok", ResourceType: "vm", ResourceID: "vm-1"})
	restarted.wait(fresh.ID, models.TaskSucceeded)
}

func TestGracefulShutdownInterruptsTasks(t *testing.T) {
	f := newFixture(t, openDB(t, filepath.Join(t.TempDir(), "agent.db")), 1, 16)
	running := f.submit(tasks.Request{Type: "test.block"})
	f.waitStarted()
	f.wait(running.ID, models.TaskRunning)
	queued := f.submit(tasks.Request{Type: "test.ok"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.engine.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	// Nobody asked for these to be cancelled: they are interrupted.
	for _, id := range []string{running.ID, queued.ID} {
		task, err := f.engine.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status != models.TaskInterrupted || task.ErrorCode != tasks.CodeAgentShutdown {
			t.Errorf("task %s after shutdown = %s / %s", id, task.Status, task.ErrorCode)
		}
	}
	if f.runs.Load() != 1 {
		t.Errorf("%d handlers ran; the queued task must not start during shutdown", f.runs.Load())
	}

	_, _, err := f.engine.Submit(context.Background(), tasks.Request{Type: "test.ok", Actor: andy, PeerUID: peerUID})
	wantCode(t, err, models.CodeShuttingDown)
}

func TestShutdownTimesOutOnStubbornTask(t *testing.T) {
	f := newDefaultFixture(t)
	task := f.submit(tasks.Request{Type: "test.stubborn"})
	f.waitStarted()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := f.engine.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("shutdown error = %v, want a deadline error", err)
	}
	// Still running in the database: the next start's sweep will catch it.
	if got, _ := f.engine.Get(context.Background(), task.ID); got.Status != models.TaskRunning {
		t.Errorf("status = %s", got.Status)
	}
	close(f.release)
}

func TestQueueFull(t *testing.T) {
	f := newFixture(t, openDB(t, filepath.Join(t.TempDir(), "agent.db")), 1, 2)
	f.submit(tasks.Request{Type: "test.block"})
	f.waitStarted()
	keyed := tasks.Request{Type: "test.ok", IdempotencyKey: "k1"}
	first := f.submit(keyed)
	f.submit(tasks.Request{Type: "test.ok"})

	_, _, err := f.engine.Submit(context.Background(), tasks.Request{Type: "test.ok", Actor: andy, PeerUID: peerUID})
	wantCode(t, err, models.CodeQueueFull)

	// A replay creates nothing, so it is still answered.
	keyed.Actor, keyed.PeerUID = andy, peerUID
	replay, created, err := f.engine.Submit(context.Background(), keyed)
	if err != nil || created || replay.ID != first.ID {
		t.Errorf("replay on a full queue: id=%s created=%v err=%v", replay.ID, created, err)
	}
	// A new key cannot be accepted.
	keyed.IdempotencyKey = "k2"
	_, _, err = f.engine.Submit(context.Background(), keyed)
	wantCode(t, err, models.CodeQueueFull)
	close(f.release)
}

func TestListPagination(t *testing.T) {
	f := newDefaultFixture(t)
	ctx := context.Background()
	var all []string
	for range 7 {
		task := f.submit(tasks.Request{Type: "test.ok"})
		f.wait(task.ID, models.TaskSucceeded)
		all = append(all, task.ID)
		time.Sleep(2 * time.Millisecond) // distinct created_at values
	}
	failed := f.submit(tasks.Request{Type: "test.fail"})
	f.wait(failed.ID, models.TaskFailed)

	var got []string
	cursor := ""
	pages := 0
	for {
		page, next, err := f.engine.List(ctx, models.TaskListOptions{Status: models.TaskSucceeded, Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, task := range page {
			got = append(got, task.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if pages != 3 || len(got) != 7 {
		t.Fatalf("pages = %d, tasks = %d; want 3 and 7", pages, len(got))
	}
	for i, id := range got { // newest first
		if id != all[len(all)-1-i] {
			t.Fatalf("page order is wrong at %d", i)
		}
	}

	page, next, err := f.engine.List(ctx, models.TaskListOptions{})
	if err != nil || len(page) != 8 || next != "" || page[0].ID != failed.ID {
		t.Errorf("default list: %d tasks, next=%q, err=%v", len(page), next, err)
	}

	for name, opts := range map[string]models.TaskListOptions{
		"bad status":     {Status: "done"},
		"negative limit": {Limit: -1},
		"huge limit":     {Limit: tasks.MaxPageSize + 1},
		"bad cursor":     {Cursor: "not-a-cursor"},
	} {
		if _, _, err := f.engine.List(ctx, opts); err == nil {
			t.Errorf("%s: accepted", name)
		} else {
			wantCode(t, err, models.CodeInvalidRequest)
		}
	}
}

func TestRegisterValidation(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "agent.db"))
	e, err := tasks.New(tasks.Options{Store: database.NewTaskStore(db), Audit: database.NewAuditLog(db), NodeID: localNode(t, db),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	run := func(context.Context, tasks.Task, tasks.Reporter) (any, error) { return nil, nil }
	for name, def := range map[string]tasks.Definition{
		"bad type":   {Type: "Nope", Timeout: time.Second, Run: run},
		"no dot":     {Type: "refresh", Timeout: time.Second, Run: run},
		"no handler": {Type: "a.b", Timeout: time.Second},
		"no timeout": {Type: "a.b", Run: run},
	} {
		if err := e.Register(def); err == nil {
			t.Errorf("%s: registered", name)
		}
	}
	good := tasks.Definition{Type: "a.b", Timeout: time.Second, Run: run}
	if err := e.Register(good); err != nil {
		t.Fatal(err)
	}
	if err := e.Register(good); err == nil {
		t.Error("a duplicate type was registered")
	}
	if _, _, err := e.Submit(context.Background(), tasks.Request{Type: "a.b", Actor: andy}); err == nil {
		t.Error("a task was accepted before Start")
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.Register(tasks.Definition{Type: "c.d", Timeout: time.Second, Run: run}); err == nil {
		t.Error("a type was registered after Start")
	}
	if err := e.Start(context.Background()); err == nil {
		t.Error("the engine started twice")
	}
	_ = e.Shutdown(context.Background())

	if _, err := tasks.New(tasks.Options{NodeID: "n"}); err == nil {
		t.Error("an engine without a store was created")
	}
}

// D40 (step 8e-1): a forwarded request's origin node and envelope are in
// the details of the "requested" audit entry of its task, of a refusal,
// and of a cancellation; a local request's entries have none.
func TestForwardedDetailsAreStored(t *testing.T) {
	f := newDefaultFixture(t)
	origin, env := "a1a1a1a1-0000-4000-8000-00000000000a", "v1.payload.sig"
	task := f.submit(tasks.Request{Type: "test.block", OriginNode: origin, Envelope: env})
	f.waitStarted()
	if _, err := f.engine.Cancel(context.Background(), task.ID, tasks.CancelRequest{Actor: andy, PeerUID: peerUID, OriginNode: origin, Envelope: env}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.engine.Submit(context.Background(), tasks.Request{Type: "test.validated", Actor: andy, OriginNode: origin, Envelope: env}); err == nil {
		t.Fatal("an invalid payload was accepted")
	}
	local := f.submit(tasks.Request{Type: "test.ok"})
	want := `{"envelope":"v1.payload.sig","origin_node":"` + origin + `"}`
	for _, c := range []struct{ where, details string }{
		{"action = 'test.block' AND phase = 'requested'", want},
		{"action = 'task.cancel'", `{"envelope":"v1.payload.sig","origin_node":"` + origin + `","task_status":"running","task_type":"test.block"}`},
	} {
		var d string
		if err := f.db.QueryRow(`SELECT COALESCE(details, '') FROM audit_log WHERE ` + c.where).Scan(&d); err != nil || d != c.details {
			t.Errorf("%s: %q %v", c.where, d, err)
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'test.validated' AND details LIKE '%"origin_node":"` + origin + `"%'`); n != 1 {
		t.Errorf("the refusal's details: %d", n)
	}
	var d string
	_ = f.db.QueryRow(`SELECT COALESCE(details, '') FROM audit_log WHERE task_id = ? AND phase = 'requested'`, local.ID).Scan(&d)
	if strings.Contains(d, "origin_node") {
		t.Errorf("a local task's entry: %q", d)
	}
}
