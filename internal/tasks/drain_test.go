package tasks_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
)

// drainFixture is an engine with a clock the test moves.
type drainFixture struct {
	*fixture
	mu  sync.Mutex
	now time.Time
}

func (d *drainFixture) clock() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.now
}

func (d *drainFixture) advance(by time.Duration) {
	d.mu.Lock()
	d.now = d.now.Add(by)
	d.mu.Unlock()
}

func newDrainFixture(t *testing.T) *drainFixture {
	t.Helper()
	db := openDB(t, filepath.Join(t.TempDir(), "agent.db"))
	d := &drainFixture{fixture: &fixture{t: t, db: db}, now: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	e, err := tasks.New(tasks.Options{Store: database.NewTaskStore(db), Audit: database.NewAuditLog(db), NodeID: localNode(t, db),
		Workers: 1, QueueSize: 8, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: d.clock})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Register(tasks.Definition{Type: "test.ok", Timeout: time.Minute,
		Run: func(context.Context, tasks.Task, tasks.Reporter) (any, error) { return nil, nil }}); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.Shutdown(ctx)
	})
	d.engine = e
	return d
}

var rootDrain = tasks.DrainActor{Actor: models.TaskActor{ID: "root", Name: "root"}, PeerUID: 0, RequestID: "r-1"}

// V6 (engine part): a drain refuses a new task with node_draining and
// still answers a replay by idempotency key; cancel is not affected.
func TestDrainRefusesNewTasksAndAnswersReplays(t *testing.T) {
	d := newDrainFixture(t)
	ctx := context.Background()
	first := d.submit(tasks.Request{Type: "test.ok", IdempotencyKey: "k-1"})

	got, n, err := d.engine.SetDrain(ctx, tasks.DrainUpgrade, rootDrain)
	if err != nil || got.Reason != tasks.DrainUpgrade || !got.ExpiresAt.Equal(d.clock().Add(tasks.DrainFor)) || n < 0 {
		t.Fatalf("set: %+v %d %v", got, n, err)
	}
	// A replay is answered with the first task.
	again, created, err := d.engine.Submit(ctx, tasks.Request{Type: "test.ok", IdempotencyKey: "k-1", Actor: andy, PeerUID: peerUID})
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("replay during a drain: %+v %v %v", again, created, err)
	}
	// Anything else is refused, recorded as a failed submission.
	before := d.count(`SELECT COUNT(*) FROM tasks`)
	_, _, err = d.engine.Submit(ctx, tasks.Request{Type: "test.ok", IdempotencyKey: "k-2", Actor: andy, PeerUID: peerUID})
	me := wantCode(t, err, models.CodeNodeDraining)
	if me.Details["reason"] != tasks.DrainUpgrade || me.Details["expires_at"] == nil {
		t.Fatalf("details: %+v", me.Details)
	}
	if d.count(`SELECT COUNT(*) FROM tasks`) != before {
		t.Fatal("a refused submission created a task")
	}
	if n := d.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'test.ok' AND phase = 'requested' AND outcome = 'failure'`); n != 1 {
		t.Fatalf("refused submissions audited: %d", n)
	}
	// Cancel is not affected.
	if _, err := d.engine.Cancel(ctx, first.ID, tasks.CancelRequest{Actor: andy}); err != nil {
		t.Fatalf("cancel during a drain: %v", err)
	}
}

// D3–D5: the same reason renews, another is refused, the clock expires
// it, clearing is idempotent, and each change is audited once.
func TestDrainRenewExpireClearAndAudit(t *testing.T) {
	d := newDrainFixture(t)
	ctx := context.Background()
	if _, _, err := d.engine.SetDrain(ctx, "maintenance", rootDrain); err == nil {
		t.Fatal("reason maintenance was accepted")
	}
	if _, _, err := d.engine.SetDrain(ctx, tasks.DrainJoin, rootDrain); err != nil {
		t.Fatal(err)
	}
	d.advance(30 * time.Second)
	renewed, _, err := d.engine.SetDrain(ctx, tasks.DrainJoin, rootDrain)
	if err != nil || !renewed.ExpiresAt.Equal(d.clock().Add(tasks.DrainFor)) {
		t.Fatalf("renew: %+v %v", renewed, err)
	}
	_, _, err = d.engine.SetDrain(ctx, tasks.DrainUpgrade, rootDrain)
	wantCode(t, err, models.CodeDrainActive)
	if cur, ok, _ := d.engine.CurrentDrain(ctx); !ok || cur.Reason != tasks.DrainJoin {
		t.Fatalf("another reason changed the drain: %+v %v", cur, ok)
	}
	d.advance(tasks.DrainFor)
	if _, ok, _ := d.engine.CurrentDrain(ctx); ok {
		t.Fatal("the drain did not expire")
	}
	if _, created, err := d.engine.Submit(ctx, tasks.Request{Type: "test.ok", Actor: andy, PeerUID: peerUID}); err != nil || !created {
		t.Fatalf("submit after expiry: %v %v", created, err)
	}
	d.engine.ClearDrain(ctx, rootDrain) // no drain: no change
	if _, _, err := d.engine.SetDrain(ctx, tasks.DrainUpgrade, rootDrain); err != nil {
		t.Fatal(err)
	}
	d.engine.ClearDrain(ctx, rootDrain)
	if _, ok, _ := d.engine.CurrentDrain(ctx); ok {
		t.Fatal("clear left the drain")
	}
	for action, want := range map[string]int{tasks.ActionDrainSet: 2, tasks.ActionDrainExpired: 1, tasks.ActionDrainCleared: 1} {
		if n := d.count(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, action); n != want {
			t.Errorf("%s: %d entries, want %d", action, n, want)
		}
	}
	rows := d.auditRows(`action = ?`, tasks.ActionDrainSet)
	if rows[0].actorName != "root" || rows[0].peerUID != 0 {
		t.Errorf("set entry: %+v", rows[0])
	}
}

// heldEngine is a one-worker engine whose worker stops after taking its
// first task off the queue, until release is closed; held receives the
// ID of every task taken off the queue.
func heldEngine(t *testing.T) (f *fixture, held chan string, release chan struct{}) {
	db := openDB(t, filepath.Join(t.TempDir(), "agent.db"))
	f = &fixture{t: t, db: db}
	e, err := tasks.New(tasks.Options{Store: database.NewTaskStore(db), Audit: database.NewAuditLog(db), NodeID: localNode(t, db),
		Workers: 1, QueueSize: 8, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Register(tasks.Definition{Type: "test.ok", Timeout: time.Minute, Cancellable: true,
		Run: func(context.Context, tasks.Task, tasks.Reporter) (any, error) { return nil, nil }}); err != nil {
		t.Fatal(err)
	}
	held, release = make(chan string, 4), make(chan struct{})
	var once sync.Once
	tasks.SetDequeuedHook(e, func(id string) {
		held <- id
		once.Do(func() { <-release })
	})
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.Shutdown(ctx)
	})
	f.engine = e
	return f, held, release
}

func unfinishedOf(e *tasks.Engine) int { _, _, n := e.CurrentDrain(context.Background()); return n }

// F6 of the 8d-1 review: a task a worker has taken off the queue and not
// yet started is unfinished; so is one queued behind it, until it is
// cancelled; a finished task is not.
func TestDrainCountsATaskBetweenQueueAndRunning(t *testing.T) {
	f, held, release := heldEngine(t)
	e, ctx := f.engine, context.Background()
	unfinished := func() int { return unfinishedOf(e) }

	first := f.submit(tasks.Request{Type: "test.ok"})
	if id := <-held; id != first.ID {
		t.Fatalf("held %s, want %s", id, first.ID)
	}
	// Off the queue, not in running: still counted.
	if n := unfinished(); n != 1 {
		t.Fatalf("unfinished with the task between the queue and running: %d, want 1", n)
	}
	second := f.submit(tasks.Request{Type: "test.ok"})
	if n := unfinished(); n != 2 {
		t.Fatalf("unfinished with one held and one queued: %d, want 2", n)
	}
	// Cancelled while pending: no longer counted, at once.
	if _, err := e.Cancel(ctx, second.ID, tasks.CancelRequest{Actor: andy, PeerUID: peerUID}); err != nil {
		t.Fatal(err)
	}
	if n := unfinished(); n != 1 {
		t.Fatalf("unfinished after cancelling the queued task: %d, want 1", n)
	}
	close(release)
	f.wait(first.ID, models.TaskSucceeded)
	deadline := time.Now().Add(5 * time.Second)
	for unfinished() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("unfinished after both ended: %d, want 0", unfinished())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// F6: tasks still queued at shutdown are interrupted and no longer
// counted.
func TestDrainCountAfterShutdown(t *testing.T) {
	f, held, release := heldEngine(t)
	first := f.submit(tasks.Request{Type: "test.ok"})
	<-held
	second := f.submit(tasks.Request{Type: "test.ok"})
	if n := unfinishedOf(f.engine); n != 2 {
		t.Fatalf("unfinished: %d, want 2", n)
	}
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- f.engine.Shutdown(ctx)
	}()
	// Release the worker only once the engine is stopping.
	for {
		_, _, err := f.engine.Submit(context.Background(), tasks.Request{Type: "test.ok", Actor: andy, PeerUID: peerUID})
		var me *models.Error
		if errors.As(err, &me) && me.Code == models.CodeShuttingDown {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first.ID, second.ID} {
		if task, err := f.engine.Get(context.Background(), id); err != nil || task.Status != models.TaskInterrupted {
			t.Errorf("%s: %+v %v", id, task.Status, err)
		}
	}
	if n := unfinishedOf(f.engine); n != 0 {
		t.Fatalf("unfinished after shutdown: %d, want 0", n)
	}
}
