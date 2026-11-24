package containers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/hostops/hostfake"
	"github.com/greenbeanorg/skep/pkg/models"
)

// memAudit keeps what the sweeps write, apart from the tasks' entries.
type memAudit struct{ entries []audit.Entry }

func (m *memAudit) Write(_ context.Context, e audit.Entry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	m.entries = append(m.entries, e)
	return nil
}

func (m *memAudit) one(t *testing.T, action, id string, want audit.Outcome) audit.Entry {
	t.Helper()
	var found []audit.Entry
	for _, e := range m.entries {
		if e.Action == action && e.ResourceID == id {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s on %s: %d entries, want 1 (all: %+v)", action, id, len(found), m.entries)
	}
	e := found[0]
	if e.ActorID != audit.System || e.ActorName != audit.System || e.Phase != audit.Finished ||
		e.Outcome != want || e.ResourceType != "container" {
		t.Fatalf("entry %+v, want outcome %s", e, want)
	}
	return e
}

func auditedFixture(t *testing.T) (*fixture, *memAudit) {
	m := &memAudit{}
	return newFixture(t, func(o *Options) { o.Audit = m }), m
}

func TestSweepAuditHalfStarted(t *testing.T) {
	f, m := auditedFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "half", TemplateID: f.minimal})
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	if err := database.NewContainerStore(f.db).MarkStarting(ctx, c.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ObserveAll(ctx); err != nil {
		t.Fatal(err)
	}
	m.one(t, actionSweepHalfStarted, c.ID, audit.Success)
}

func TestSweepAuditHalfStartedFailureHasNoErrorText(t *testing.T) {
	f, m := auditedFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "half", TemplateID: f.minimal})
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	if err := database.NewContainerStore(f.db).MarkStarting(ctx, c.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	f.fake.Set(func(fk *hostfake.Fake) { fk.FailDiscard = errors.New("secret-path busy") })
	if err := f.svc.ObserveAll(ctx); err != nil {
		t.Fatal(err)
	}
	e := m.one(t, actionSweepHalfStarted, c.ID, audit.Failure)
	if r, _ := e.Details["reason"].(string); r == "" || strings.Contains(r, "secret-path") {
		t.Fatalf("reason %q", r)
	}
}

func TestSweepAuditQuarantine(t *testing.T) {
	f, m := auditedFixture(t)
	ctx := context.Background()
	c := f.quarantined("q")
	f.fake.Set(func(fk *hostfake.Fake) { fk.FailDiscard = errors.New("busy") })
	if err := f.svc.ObserveAll(ctx); err != nil {
		t.Fatal(err)
	}
	m.one(t, actionSweepQuarantine, c.ID, audit.Failure)
	m.entries = nil
	f.fake.Set(func(fk *hostfake.Fake) { fk.FailDiscard = nil })
	if err := f.svc.ObserveAll(ctx); err != nil {
		t.Fatal(err)
	}
	m.one(t, actionSweepQuarantine, c.ID, audit.Success)
}

func TestSweepAuditStopCompleted(t *testing.T) {
	f, m := auditedFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "s", TemplateID: f.minimal})
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	f.interruptStop(c.ID, true)
	if err := f.svc.ObserveAll(ctx); err != nil {
		t.Fatal(err)
	}
	m.one(t, actionSweepStopCompleted, c.ID, audit.Success)
}

func TestSweepAuditDevfsOnlyWhenSomethingWasRemoved(t *testing.T) {
	f, m := auditedFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "d", TemplateID: f.minimal})
	if err := f.svc.ObserveAll(ctx); err != nil {
		t.Fatal(err)
	}
	if len(m.entries) != 0 {
		t.Fatalf("a sweep that did nothing wrote %+v", m.entries)
	}
	f.fake.LeaveDevfs(c.ID)
	if err := f.svc.ObserveAll(ctx); err != nil {
		t.Fatal(err)
	}
	m.one(t, actionSweepDevfs, c.ID, audit.Success)
}

// A devfs that was found and could not be unmounted is a failure entry
// with a fixed reason, although nothing was removed.
func TestSweepAuditDevfsUnmountFailed(t *testing.T) {
	f, m := auditedFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "d", TemplateID: f.minimal})
	f.fake.LeaveDevfs(c.ID)
	f.fake.StickDevfs(c.ID)
	if err := f.svc.ObserveAll(ctx); err != nil {
		t.Fatal(err)
	}
	e := m.one(t, actionSweepDevfs, c.ID, audit.Failure)
	if e.Details["reason"] != "a leftover devfs mount could not be removed; see the agent's log" {
		t.Fatalf("details %+v", e.Details)
	}
}

// A check that failed before finding a devfs writes no entry.
func TestSweepAuditDevfsCheckFailedWritesNone(t *testing.T) {
	f, m := auditedFixture(t)
	c := f.create(models.ContainerCreate{Name: "d", TemplateID: f.minimal})
	c.Pool = "no-such-pool"
	f.svc.sweepLeftoverDevfs(context.Background(), c)
	if len(m.entries) != 0 {
		t.Fatalf("a failed check wrote %+v", m.entries)
	}
}

// The entry is written even when the agent is shutting down.
func TestSweepAuditSurvivesACancelledContext(t *testing.T) {
	f, m := auditedFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.svc.auditSweep(ctx, "c1", actionSweepStopCompleted, audit.Failure, "x")
	var seen bool
	for _, e := range m.entries {
		seen = seen || e.ResourceID == "c1"
	}
	if !seen {
		t.Fatal("no entry")
	}
}
