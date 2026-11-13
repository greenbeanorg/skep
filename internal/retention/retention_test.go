package retention_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/auditarchive"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/retention"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/migrations"
	"github.com/greenbeanorg/skep/pkg/models"
)

// clock is the injected clock, shared by the engine, the archive, and the
// retention task. No test changes the system clock.
type clock struct{ ns atomic.Int64 }

func (c *clock) now() time.Time { return time.Unix(0, c.ns.Load()).UTC() }

func (c *clock) set(s string) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	c.ns.Store(t.UnixNano())
}

type fixture struct {
	t       *testing.T
	db      *sql.DB
	clock   *clock
	engine  *tasks.Engine
	archive *auditarchive.Archiver
	dir     string
}

type settings struct {
	tasksDays, liveDays int
	noArchive           bool
}

func newFixture(t *testing.T, s settings) *fixture {
	t.Helper()
	ctx := context.Background()
	state := filepath.Join(t.TempDir(), "agent")
	db, err := database.Open(ctx, filepath.Join(state, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := database.Migrate(ctx, db, migrations.Agent()); err != nil {
		t.Fatal(err)
	}
	nodeID, err := database.EnsureLocalNode(ctx, db, "hearst")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, db: db, clock: &clock{}, dir: filepath.Join(state, "audit-archive")}
	f.clock.set("2026-10-20T12:00:00Z")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if !s.noArchive {
		f.archive, err = auditarchive.New(auditarchive.Options{
			DB: db, Log: auditarchive.AgentLog, Dir: f.dir, Logger: log, Now: f.clock.now,
			NodeID: func(context.Context) (string, error) { return nodeID, nil },
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	store := database.NewTaskStore(db)
	runner, err := retention.New(retention.Options{
		Tasks: store, TasksDays: s.tasksDays, Archive: f.archive, AuditLiveDays: s.liveDays,
		Logger: log, Now: f.clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.engine, err = tasks.New(tasks.Options{
		Store: store, Audit: database.NewAuditLog(db), NodeID: nodeID, Logger: log, Now: f.clock.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Register(runner.Definition()); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		f.engine.Shutdown(ctx)
	})
	return f
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
}

// oldTask inserts a finished task with two events.
func (f *fixture) oldTask(id, finishedAt string) {
	f.t.Helper()
	f.exec(`INSERT INTO tasks (id, type, actor_id, actor_name, peer_uid, request_hash) VALUES (?, 'x.y', 'u1', 'andy', 1001, 'h')`, id)
	f.exec(`INSERT INTO task_events (task_id, message) VALUES (?, 'one'), (?, 'two')`, id, id)
	f.exec(`UPDATE tasks SET status = 'running' WHERE id = ?`, id)
	f.exec(`UPDATE tasks SET status = 'succeeded', finished_at = ? WHERE id = ?`, finishedAt, id)
}

func (f *fixture) auditEntries(at string, n int) {
	f.t.Helper()
	for range n {
		f.exec(`INSERT INTO audit_log (occurred_at, actor_id, actor_name, peer_uid, action, outcome) VALUES (?, 'u1', 'andy', 1001, 'vm.start', 'success')`, at)
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

// run submits one retention task, as the agent's timer does, and waits for
// it to finish.
func (f *fixture) run() tasks.Task {
	f.t.Helper()
	ctx := context.Background()
	task, created, err := f.engine.Submit(ctx, tasks.Request{
		Type: retention.TaskType, ResourceType: retention.ResourceType, ResourceID: retention.ResourceID,
		Actor: models.TaskActor{ID: audit.System, Name: audit.System}, PeerUID: uint32(os.Geteuid()),
	})
	if err != nil || !created {
		f.t.Fatalf("Submit = %v, %v", created, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := f.engine.Get(ctx, task.ID)
		if err != nil {
			f.t.Fatal(err)
		}
		if got.Status.Terminal() {
			return got
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("the retention task did not finish: %+v", got)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// finished returns the details of a task's "finished" audit entry.
func (f *fixture) finished(taskID string) map[string]any {
	f.t.Helper()
	var actor, outcome, raw string
	if err := f.db.QueryRow(`SELECT actor_id, outcome, COALESCE(details, '{}') FROM audit_log
		WHERE task_id = ? AND phase = 'finished'`, taskID).Scan(&actor, &outcome, &raw); err != nil {
		f.t.Fatal(err)
	}
	if actor != audit.System {
		f.t.Errorf("finished entry actor = %q, want system", actor)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		f.t.Fatal(err)
	}
	d["outcome"] = outcome
	return d
}

func result(t *testing.T, task tasks.Task) retention.Result {
	t.Helper()
	var r retention.Result
	if err := json.Unmarshal(task.Result, &r); err != nil {
		t.Fatalf("result %s: %v", task.Result, err)
	}
	return r
}

// TestRunAcrossAMonthBoundary runs the task, as the engine runs it, on
// both sides of midnight UTC at the end of a month, with the agent's
// default settings: tasks 90 days, audit entries sealed and never removed.
func TestRunAcrossAMonthBoundary(t *testing.T) {
	f := newFixture(t, settings{tasksDays: 90})
	f.oldTask("old", "2026-08-02T23:59:57.000Z")   // one second older than 90 days at the first run
	f.oldTask("young", "2026-08-03T00:00:01.000Z") // three seconds too young at the first run
	f.auditEntries("2026-09-10T12:00:00.000Z", 4)  // 1-4
	f.auditEntries("2026-10-10T12:00:00.000Z", 3)  // 5-7

	// Two seconds before the month ends.
	f.clock.set("2026-10-31T23:59:58Z")
	first := f.run()
	if first.Status != models.TaskSucceeded || first.Cancellable {
		t.Fatalf("first run = %+v", first)
	}
	r := result(t, first)
	if r.Tasks.TasksRemoved != 1 || r.Tasks.EventsRemoved != 2 || r.Tasks.RetentionDays != 90 ||
		r.Tasks.Cutoff == nil || !r.Tasks.Cutoff.Equal(time.Date(2026, 8, 2, 23, 59, 58, 0, time.UTC)) {
		t.Errorf("first run tasks = %+v", r.Tasks)
	}
	if r.Audit == nil || len(r.Audit.Sealed) != 1 || r.Audit.Sealed[0].Month != "2026-09" ||
		r.Audit.Sealed[0].FirstID != 1 || r.Audit.Sealed[0].LastID != 4 || r.Audit.Removed.Entries != 0 {
		t.Errorf("first run audit = %+v", r.Audit)
	}
	if f.count(`SELECT COUNT(*) FROM tasks WHERE id = 'old'`) != 0 || f.count(`SELECT COUNT(*) FROM tasks WHERE id = 'young'`) != 1 {
		t.Error("the wrong task was deleted")
	}

	// Removal of history is itself on the record: the finished audit entry
	// names the cutoff, the counts, and the seal.
	d := f.finished(first.ID)
	sealed, _ := d["audit_segments_sealed"].([]any)
	if d["outcome"] != "success" || d["tasks_cutoff"] != "2026-08-02T23:59:58Z" || d["tasks_removed"] != float64(1) ||
		d["events_removed"] != float64(2) || d["audit_entries_removed"] != float64(0) || len(sealed) != 1 {
		t.Errorf("finished entry of the first run = %v", d)
	}
	if seg, _ := sealed[0].(map[string]any); seg["month"] != "2026-09" || seg["sha256"] != r.Audit.Sealed[0].Hash {
		t.Errorf("seal in the finished entry = %v", sealed)
	}

	// Two seconds into the new month: October is due now, and the second
	// old task has crossed the retention age.
	f.clock.set("2026-11-01T00:00:02Z")
	second := f.run()
	r = result(t, second)
	if second.Status != models.TaskSucceeded || r.Tasks.TasksRemoved != 1 ||
		len(r.Audit.Sealed) != 1 || r.Audit.Sealed[0].Month != "2026-10" || r.Audit.Verified != 1 {
		t.Fatalf("second run = %+v, %+v, audit %+v", second.Status, r.Tasks, r.Audit)
	}
	// October's segment holds its three entries and the audit trail of
	// the first run, which was written in October: IDs 5 to 9.
	if got := r.Audit.Sealed[0]; got.FirstID != 5 || got.LastID != 9 || got.Entries != 5 {
		t.Errorf("October segment = %+v", got)
	}

	// The default never removes a live audit entry.
	if n := f.count(`SELECT COUNT(*) FROM audit_log WHERE id <= 9`); n != 9 {
		t.Errorf("%d of the 9 sealed entries are live, want all", n)
	}
	rep, err := f.archive.Verify(context.Background())
	if err != nil || rep.Err(false) != nil || len(rep.Checks) != 2 {
		t.Errorf("Verify = %+v, %v", rep, err)
	}
	if info, err := os.Stat(f.dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("archive directory: %v, %v", info, err)
	}

	// The retention tasks themselves: old runs are pruned like any task,
	// but the newest one is always kept.
	f.clock.set("2027-06-01T00:00:00Z")
	third := f.run()
	if n := f.count(`SELECT COUNT(*) FROM tasks WHERE type = ?`, retention.TaskType); n != 2 {
		t.Errorf("%d retention tasks left, want the previous newest and the one that just ran", n)
	}
	if f.count(`SELECT COUNT(*) FROM tasks WHERE id = ?`, first.ID) != 0 || f.count(`SELECT COUNT(*) FROM tasks WHERE id = ?`, second.ID) != 1 {
		t.Error("the wrong retention task was kept")
	}
	// The audit entries of the deleted run are still there.
	if f.count(`SELECT COUNT(*) FROM audit_log WHERE task_id = ?`, first.ID) != 2 {
		t.Error("the audit trail of a deleted task is gone")
	}
	if third.Status != models.TaskSucceeded {
		t.Errorf("third run = %+v", third)
	}
}

// TestOptInRemoval checks that live audit entries go only when the
// operator set a number of days, and that the task records it.
func TestOptInRemoval(t *testing.T) {
	f := newFixture(t, settings{tasksDays: 90, liveDays: 35})
	f.auditEntries("2026-09-10T12:00:00.000Z", 4)
	f.clock.set("2026-11-04T12:00:00Z") // September + 35 days is November 5th
	if r := result(t, f.run()); len(r.Audit.Sealed) != 1 || r.Audit.Removed.Entries != 0 {
		t.Fatalf("first run audit = %+v", r.Audit)
	}
	f.clock.set("2026-11-05T00:00:00Z")
	task := f.run()
	r := result(t, task)
	if r.Audit.Removed.Entries != 4 || len(r.Audit.Removed.Months) != 1 || r.Audit.Removed.Months[0] != "2026-09" {
		t.Fatalf("second run audit = %+v", r.Audit)
	}
	if n := f.count(`SELECT COUNT(*) FROM audit_log WHERE id <= 4`); n != 0 {
		t.Errorf("%d September entries still live", n)
	}
	d := f.finished(task.ID)
	if d["audit_entries_removed"] != float64(4) || len(d["audit_months_removed"].([]any)) != 1 {
		t.Errorf("finished entry = %v", d)
	}
}

// TestArchiveFailureFailsTheTaskAndKeepsEverything damages a segment and
// runs the task: it fails with a specific code, removes no audit entry,
// still prunes tasks, and records all of that.
func TestArchiveFailureFailsTheTaskAndKeepsEverything(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(path string) error
		code   string
	}{
		{"altered", func(path string) error {
			if err := os.Chmod(path, 0o600); err != nil {
				return err
			}
			return os.WriteFile(path, []byte("not a segment"), 0o600)
		}, retention.CodeArchiveInvalid},
		{"missing", os.Remove, retention.CodeArchiveMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, settings{tasksDays: 90, liveDays: 35})
			f.auditEntries("2026-08-10T12:00:00.000Z", 4)
			f.auditEntries("2026-09-10T12:00:00.000Z", 2)
			f.clock.set("2026-09-02T00:00:00Z")
			if task := f.run(); task.Status != models.TaskSucceeded {
				t.Fatalf("sealing run = %+v", task)
			}
			segs, err := f.archive.Segments(context.Background())
			if err != nil || len(segs) != 1 {
				t.Fatal(segs, err)
			}
			if err := tc.damage(filepath.Join(f.dir, segs[0].FileName)); err != nil {
				t.Fatal(err)
			}
			f.oldTask("old", "2026-01-01T00:00:00.000Z")

			// August is due for removal, September for sealing.
			f.clock.set("2026-12-01T00:00:00Z")
			live := f.count(`SELECT COUNT(*) FROM audit_log`)
			task := f.run()
			if task.Status != models.TaskFailed || task.ErrorCode != tc.code {
				t.Fatalf("task = %s, code %q; want failed, %q", task.Status, task.ErrorCode, tc.code)
			}
			// The two audit entries of this run were added; nothing left.
			if n := f.count(`SELECT COUNT(*) FROM audit_log`); n != live+2 {
				t.Errorf("%d audit entries, want %d: none may be removed", n, live+2)
			}
			if n := f.count(`SELECT COUNT(*) FROM audit_log WHERE id <= 4`); n != 4 {
				t.Errorf("%d of August's entries are live, want 4", n)
			}
			// Tasks are independent of the archive.
			if f.count(`SELECT COUNT(*) FROM tasks WHERE id = 'old'`) != 0 {
				t.Error("the old task was not pruned")
			}
			d := f.finished(task.ID)
			if d["outcome"] != "failure" || d["error_code"] != tc.code || d["tasks_removed"] != float64(1) ||
				d["audit_entries_removed"] != float64(0) {
				t.Errorf("finished entry = %v", d)
			}
			if tc.code == retention.CodeArchiveInvalid && d["audit_segments_sealed"] != nil {
				t.Errorf("a segment was sealed on top of a broken archive: %v", d)
			}
			if f.archive.Health(context.Background()) == "" {
				t.Error("the health check reports nothing")
			}
			// A failed run loses nothing: the next one starts from the
			// same rules and fails the same way until the file is back.
			if again := f.run(); again.ErrorCode != tc.code {
				t.Errorf("second run = %s %q", again.Status, again.ErrorCode)
			}
		})
	}
}

func TestSettingsThatKeepEverything(t *testing.T) {
	// tasks_days 0 and audit_archive false: option A of both decisions.
	f := newFixture(t, settings{tasksDays: 0, noArchive: true})
	f.oldTask("ancient", "2020-01-01T00:00:00.000Z")
	f.auditEntries("2020-01-01T00:00:00.000Z", 3)
	f.clock.set("2026-11-12T00:00:00Z")
	task := f.run()
	r := result(t, task)
	if task.Status != models.TaskSucceeded || r.Tasks.Cutoff != nil || r.Tasks.TasksRemoved != 0 || r.Audit != nil {
		t.Errorf("result = %+v (%s)", r, task.Status)
	}
	if f.count(`SELECT COUNT(*) FROM tasks WHERE id = 'ancient'`) != 1 || f.count(`SELECT COUNT(*) FROM audit_segments`) != 0 {
		t.Error("something was deleted or sealed")
	}
	if _, err := os.Stat(f.dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the archive directory exists: %v", err)
	}
	if d := f.finished(task.ID); d["audit_archive"] != "off" || d["tasks_retention_days"] != float64(0) {
		t.Errorf("finished entry = %v", d)
	}
}

func TestSchedule(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		retention.Schedule(ctx, 5*time.Millisecond, func(context.Context) error {
			if calls.Add(1) == 2 {
				return errors.New("busy") // a failed submission does not stop the timer
			}
			return nil
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("%d calls", calls.Load())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Schedule did not stop")
	}
}

func TestScheduleRunsAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := make(chan struct{}, 1)
	go retention.Schedule(ctx, time.Hour, func(context.Context) error {
		called <- struct{}{}
		return nil
	}, slog.Default())
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("the first run waits for the interval")
	}
}
