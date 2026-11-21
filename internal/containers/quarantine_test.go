package containers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/hostops/hostfake"
	"github.com/greenbeanorg/skep/pkg/models"
)

// The quarantine of amendment n (SR-06): a jail the start refused before
// any of its code ran, whose removal then failed. It is never shown as
// running, start and stop are refused, and nothing ever runs its
// /etc/rc.shutdown; delete and the start-up sweep remove it without
// exec.stop.

// withExecStop lists the fake's removals that would run exec.stop.
func withExecStop(calls []string, id string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "jail -r skep-"+id) && !strings.Contains(c, "no exec.stop") {
			out = append(out, c)
		}
	}
	return out
}

// quarantined starts a container whose start the fake quarantines, and
// returns it with the jail still present on the fake host.
func (f *fixture) quarantined(name string) models.Container {
	f.t.Helper()
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: name, TemplateID: f.minimal})
	f.fake.Set(func(fk *hostfake.Fake) { fk.QuarantineStart = true })
	defer f.fake.Set(func(fk *hostfake.Fake) { fk.QuarantineStart = false })
	task, err := f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub)
	if err != nil {
		f.t.Fatal(err)
	}
	if done := f.wait(task.ID); done.Status != models.TaskFailed || done.ErrorCode != models.CodeContainerQuarantined {
		f.t.Fatalf("start = %s %s %s", done.Status, done.ErrorCode, done.ErrorMessage)
	}
	return c
}

func TestQuarantinedJailIsNeverRunningAndRefusesPower(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.quarantined("q1")
	if running, _ := f.svc.isRunning(ctx, c.ID); !running {
		t.Fatal("the fake did not leave the jail in place")
	}
	// Observed again and again (a GET, a list, an agent restart's
	// observation), it stays failed, never running.
	for range 2 {
		got, err := f.svc.Container(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != models.ContainerFailed || !strings.Contains(got.StateDetail, "quarantined") && !strings.Contains(got.StateDetail, "removal failed") {
			t.Fatalf("observed: %s %q", got.State, got.StateDetail)
		}
	}
	list, err := f.svc.Containers(ctx, models.ContainerListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range list.Items {
		if it.ID == c.ID && it.State != models.ContainerFailed {
			t.Fatalf("listed as %s", it.State)
		}
	}
	for _, act := range []string{"start", "stop", "restart"} {
		_, err := f.svc.Action(ctx, c.ID, models.ContainerAction{Action: act}, f.sub)
		wantCode(t, err, models.CodeContainerQuarantined)
	}
	if got := withExecStop(f.fake.Calls(), c.ID); len(got) != 0 {
		t.Fatalf("a removal with exec.stop ran: %q", got)
	}
}

func TestDeleteRemovesQuarantinedJailWithoutExecStop(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.quarantined("q2")

	// The removal fails again: the delete fails, the container stays
	// quarantined.
	f.fake.Set(func(fk *hostfake.Fake) { fk.FailDiscard = errors.New("jail -r timed out (simulated)") })
	task, err := f.svc.DeleteContainer(ctx, c.ID, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	if done := f.wait(task.ID); done.Status != models.TaskFailed || done.ErrorCode != models.CodeContainerQuarantined {
		t.Fatalf("delete = %s %s %s", done.Status, done.ErrorCode, done.ErrorMessage)
	}
	if got, err := f.svc.Container(ctx, c.ID); err != nil || got.State != models.ContainerFailed {
		t.Fatalf("after a failed delete: %+v %v", got, err)
	}

	// Then it works: removed without exec.stop, and the container gone.
	f.fake.Set(func(fk *hostfake.Fake) { fk.FailDiscard = nil })
	f.succeed(f.svc.DeleteContainer(ctx, c.ID, f.sub))
	if _, err := f.svc.Container(ctx, c.ID); !errors.Is(err, models.ErrNotFound) {
		t.Fatalf("still there: %v", err)
	}
	if !slices.Contains(f.fake.Calls(), "jail -r skep-"+c.ID+" (no exec.stop)") {
		t.Fatalf("not removed without exec.stop: %q", f.fake.Calls())
	}
	if got := withExecStop(f.fake.Calls(), c.ID); len(got) != 0 {
		t.Fatalf("a removal with exec.stop ran: %q", got)
	}
}

func TestObserveAllRetriesQuarantinedRemoval(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.quarantined("q3")

	// The sweep's removal fails: still quarantined, not running.
	f.fake.Set(func(fk *hostfake.Fake) { fk.FailDiscard = errors.New("busy (simulated)") })
	if err := f.svc.ObserveAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.Container(ctx, c.ID); got.State != models.ContainerFailed {
		t.Fatalf("after a failed sweep: %s", got.State)
	}

	// It works at the next agent start: removed without exec.stop, the
	// container stopped with a detail that says so, and startable again.
	f.fake.Set(func(fk *hostfake.Fake) { fk.FailDiscard = nil })
	if err := f.svc.ObserveAll(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.Container(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != models.ContainerStopped || !strings.Contains(got.StateDetail, "quarantined") {
		t.Fatalf("after the sweep: %s %q", got.State, got.StateDetail)
	}
	if running, _ := f.svc.isRunning(ctx, c.ID); running {
		t.Fatal("the jail is still there")
	}
	if got := withExecStop(f.fake.Calls(), c.ID); len(got) != 0 {
		t.Fatalf("a removal with exec.stop ran: %q", got)
	}
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
}

// TestHalfStartedSweepFailureQuarantines: a jail an agent restart left
// half-started whose removal fails is quarantined, not observed running.
func TestHalfStartedSweepFailureQuarantines(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "half", TemplateID: f.minimal})
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	if err := database.NewContainerStore(f.db).MarkStarting(ctx, c.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	f.fake.Set(func(fk *hostfake.Fake) { fk.FailDiscard = errors.New("busy (simulated)") })
	if err := f.svc.ObserveAll(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.Container(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != models.ContainerFailed || !strings.Contains(got.StateDetail, "quarantined") {
		t.Fatalf("after the failed sweep: %s %q", got.State, got.StateDetail)
	}
	_, err = f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "stop"}, f.sub)
	wantCode(t, err, models.CodeContainerQuarantined)
	if got := withExecStop(f.fake.Calls(), c.ID); len(got) != 0 {
		t.Fatalf("a removal with exec.stop ran: %q", got)
	}
}

// TestStopOfJailStillRecordedStartingIsRefused: a stop task that finds the
// container still recorded starting (no start can be running: tasks on a
// container run one at a time) refuses rather than run /etc/rc.shutdown.
func TestStopOfJailStillRecordedStartingIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "st", TemplateID: f.minimal})
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	if err := database.NewContainerStore(f.db).MarkStarting(ctx, c.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	for _, act := range []string{"stop", "restart"} {
		task, err := f.svc.Action(ctx, c.ID, models.ContainerAction{Action: act}, f.sub)
		if err != nil {
			t.Fatal(err)
		}
		if done := f.wait(task.ID); done.Status != models.TaskFailed || done.ErrorCode != models.CodeContainerQuarantined {
			t.Fatalf("%s = %s %s %s", act, done.Status, done.ErrorCode, done.ErrorMessage)
		}
	}
	if got := withExecStop(f.fake.Calls(), c.ID); len(got) != 0 {
		t.Fatalf("a removal with exec.stop ran: %q", got)
	}
}

// removeFailed is what hostops returns when jail -r and then jail -R
// both failed (amendment u).
func removeFailed(id string) error {
	return &models.Error{Code: models.CodeContainerRemoveFailed,
		Message: `the jail could not be removed: jail -r failed (jail output: "jail: skep-x: /bin/sh /etc/rc.shutdown jail: failed"), and jail -R then failed (exit status 1, last line of stderr: "jail: busy"); it may still be running`,
		Details: map[string]any{"container_id": id}}
}

// A stop whose jail -r and jail -R both fail is a task failure with
// container_remove_failed and the cleaned stderr lines in its message,
// not the engine's generic task_failed (amendment u).
func TestStopRemoveFailedIsCoded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "rf1", TemplateID: f.minimal})
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	f.fake.Set(func(fk *hostfake.Fake) { fk.FailStop = removeFailed(c.ID) })
	for _, act := range []string{"stop", "restart"} {
		task, err := f.svc.Action(ctx, c.ID, models.ContainerAction{Action: act}, f.sub)
		if err != nil {
			t.Fatal(err)
		}
		done := f.wait(task.ID)
		if done.Status != models.TaskFailed || done.ErrorCode != models.CodeContainerRemoveFailed ||
			!strings.Contains(done.ErrorMessage, "jail: busy") || !strings.Contains(done.ErrorMessage, "/etc/rc.shutdown jail: failed") {
			t.Fatalf("%s = %s %s %s", act, done.Status, done.ErrorCode, done.ErrorMessage)
		}
	}
	// The jail is still there: the container is observed running.
	if got, _ := f.svc.Container(ctx, c.ID); got.State != models.ContainerRunning {
		t.Fatalf("after a failed stop: %s %q", got.State, got.StateDetail)
	}
	f.fake.Set(func(fk *hostfake.Fake) { fk.FailStop = nil })
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "stop"}, f.sub))
}

// A delete of a quarantined jail whose removal fails with
// container_remove_failed keeps that code, says the jail stays
// quarantined, and leaves the container failed (amendment u).
func TestDeleteRemoveFailedIsCoded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.quarantined("rf2")
	f.fake.Set(func(fk *hostfake.Fake) { fk.FailDiscard = removeFailed(c.ID) })
	task, err := f.svc.DeleteContainer(ctx, c.ID, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	done := f.wait(task.ID)
	if done.Status != models.TaskFailed || done.ErrorCode != models.CodeContainerRemoveFailed ||
		!strings.Contains(done.ErrorMessage, "jail: busy") || !strings.Contains(done.ErrorMessage, "stays quarantined") {
		t.Fatalf("delete = %s %s %s", done.Status, done.ErrorCode, done.ErrorMessage)
	}
	if got, err := f.svc.Container(ctx, c.ID); err != nil || got.State != models.ContainerFailed {
		t.Fatalf("after a failed delete: %+v %v", got, err)
	}
}
