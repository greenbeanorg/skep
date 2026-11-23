package containers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/hostops/hostfake"
	"github.com/greenbeanorg/skep/pkg/models"
)

// interruptStop leaves a container as an agent restart during its stop
// leaves it (amendment aa): desired stopped, recorded stopping, and the
// epairs destroyed while the jail still runs (dropEpairs), or not.
func (f *fixture) interruptStop(id string, dropEpairs bool) {
	f.t.Helper()
	ctx := context.Background()
	st := database.NewContainerStore(f.db)
	c, err := st.Get(ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.SetDesired(ctx, id, models.ContainerStopped, now); err != nil {
		f.t.Fatal(err)
	}
	if err := st.SaveObserved(ctx, id, models.ContainerStopping, c.JID, "", now); err != nil {
		f.t.Fatal(err)
	}
	if dropEpairs {
		if _, err := f.fake.Ops().Jails.DestroyJailInterfaces(ctx, id); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fixture) epairsOf(id string) []string {
	var out []string
	for name, desc := range f.fake.Epairs() {
		if strings.Contains(desc, id) {
			out = append(out, name)
		}
	}
	return out
}

func TestObserveAllCompletesInterruptedStop(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	nic := []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: static("192.0.2.10/24", "")}}
	half := f.create(models.ContainerCreate{Name: "half", TemplateID: f.minimal, NICs: nic})
	other := f.create(models.ContainerCreate{Name: "other", TemplateID: f.minimal,
		NICs: []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: static("192.0.2.11/24", "")}}})
	f.succeed(f.svc.Action(ctx, half.ID, models.ContainerAction{Action: "start"}, f.sub))
	f.succeed(f.svc.Action(ctx, other.ID, models.ContainerAction{Action: "start"}, f.sub))
	f.interruptStop(half.ID, true)
	if rec, _ := database.NewContainerStore(f.db).Get(ctx, half.ID); rec.NICs[0].HostInterface == "" {
		t.Fatal("setup: no host_interface recorded before the restart")
	}
	if err := f.svc.ObserveAll(ctx); err != nil {
		t.Fatal(err)
	}
	running, err := f.fake.Ops().Jails.RunningJails(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := running[half.ID]; ok {
		t.Fatal("the half-stopped jail still runs")
	}
	if _, ok := running[other.ID]; !ok {
		t.Fatal("another running jail was removed")
	}
	if len(withExecStop(f.fake.Calls(), half.ID)) == 0 {
		t.Fatalf("the stop was not completed with exec.stop: %q", f.fake.Calls())
	}
	if e := f.epairsOf(half.ID); len(e) != 0 {
		t.Fatalf("epairs left: %v", e)
	}
	got, err := f.svc.Container(ctx, half.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != models.ContainerStopped || got.JID != nil || got.DesiredState != models.ContainerStopped ||
		got.NICs[0].HostInterface != "" || !strings.Contains(got.StateDetail, "completed at agent start") {
		t.Fatalf("after the restart: %+v", got)
	}
	// The detail stays on later reads while the jail stays stopped.
	if again, _ := f.svc.Container(ctx, half.ID); !strings.Contains(again.StateDetail, "completed at agent start") {
		t.Fatalf("detail lost on the next read: %q", again.StateDetail)
	}
	if got, _ := f.svc.Container(ctx, other.ID); got.State != models.ContainerRunning || got.NICs[0].HostInterface == "" {
		t.Fatalf("the other jail: %+v", got)
	}
	// It starts again normally, and a later stop clears the detail.
	f.succeed(f.svc.Action(ctx, half.ID, models.ContainerAction{Action: "start"}, f.sub))
	f.succeed(f.svc.Action(ctx, half.ID, models.ContainerAction{Action: "stop"}, f.sub))
	if got, _ := f.svc.Container(ctx, half.ID); got.State != models.ContainerStopped || got.StateDetail != "" {
		t.Fatalf("after a normal start and stop: %s %q", got.State, got.StateDetail)
	}
}

// TestObserveAllCompletesStopOfGoneJail: jail -r finished but the agent
// died before recording it; an epair was left. The completion destroys it.
func TestObserveAllCompletesStopOfGoneJail(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "gone", TemplateID: f.minimal})
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	f.interruptStop(c.ID, false)
	if err := f.fake.Ops().Jails.DiscardJail(ctx, f.svc.spec(c, 0)); err != nil {
		t.Fatal(err)
	}
	left := f.fake.LeaveEpair(c.ID, 0)
	if err := f.svc.ObserveAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.fake.Epairs()[left]; ok {
		t.Fatalf("the leftover epair %s is still there", left)
	}
	if got, _ := f.svc.Container(ctx, c.ID); got.State != models.ContainerStopped || !strings.Contains(got.StateDetail, "completed at agent start") {
		t.Fatalf("after the restart: %s %q", got.State, got.StateDetail)
	}
}

// TestObserveAllInterruptedStopFailure: the completion fails; the container
// is recorded unknown with a detail, and a later stop works.
func TestObserveAllInterruptedStopFailure(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "busy", TemplateID: f.minimal})
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	f.interruptStop(c.ID, true)
	f.fake.Set(func(fk *hostfake.Fake) { fk.FailStop = errors.New("jail -r failed (simulated)") })
	if err := f.svc.ObserveAll(ctx); err != nil {
		t.Fatal(err)
	}
	rec, err := database.NewContainerStore(f.db).Get(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != models.ContainerUnknown || !strings.Contains(rec.StateDetail, "could not be completed at agent start") {
		t.Fatalf("after the failed completion: %s %q", rec.State, rec.StateDetail)
	}
	f.fake.Set(func(fk *hostfake.Fake) { fk.FailStop = nil })
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "stop"}, f.sub))
	if got, _ := f.svc.Container(ctx, c.ID); got.State != models.ContainerStopped {
		t.Fatalf("after a later stop: %s", got.State)
	}
}

// TestObserveAllFailedCompletionDetailOnRead: the completion fails and the
// jail still runs; a read observes it running and keeps the recorded
// detail, which says why (Part 1 of the #57 fix round).
func TestObserveAllFailedCompletionDetailOnRead(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "stuck", TemplateID: f.minimal})
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	f.interruptStop(c.ID, true)
	f.fake.Set(func(fk *hostfake.Fake) { fk.FailStop = errors.New("jail -r failed (simulated)") })
	if err := f.svc.ObserveAll(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, err := f.svc.Container(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != models.ContainerRunning || got.DesiredState != models.ContainerStopped || got.JID == nil ||
			!strings.Contains(got.StateDetail, "could not be completed at agent start") {
			t.Fatalf("read %d: %s / %s %q", i, got.State, got.DesiredState, got.StateDetail)
		}
	}
	list, err := f.svc.Containers(ctx, models.ContainerListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || !strings.Contains(list.Items[0].StateDetail, "could not be completed at agent start") {
		t.Fatalf("list: %+v", list.Items)
	}
	// A successful stop clears it.
	f.fake.Set(func(fk *hostfake.Fake) { fk.FailStop = nil })
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "stop"}, f.sub))
	if got, _ := f.svc.Container(ctx, c.ID); got.State != models.ContainerStopped || got.StateDetail != "" {
		t.Fatalf("after a stop: %s %q", got.State, got.StateDetail)
	}
}

// TestFailedStopTaskDetailOnRead: a stop task that fails leaves the jail
// running, recorded unknown with "the last stop failed: ..."; reads keep
// that detail while the jail runs.
func TestFailedStopTaskDetailOnRead(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "nostop", TemplateID: f.minimal})
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	f.fake.Set(func(fk *hostfake.Fake) { fk.FailStop = errors.New("jail -R failed (simulated)") })
	task, err := f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "stop"}, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	if done := f.wait(task.ID); done.Status != models.TaskFailed {
		t.Fatalf("stop task: %s", done.Status)
	}
	got, err := f.svc.Container(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != models.ContainerRunning || !strings.HasPrefix(got.StateDetail, "the last stop failed: ") {
		t.Fatalf("after a failed stop: %s %q", got.State, got.StateDetail)
	}
}

// TestRunningAgainstDesiredStoppedDefaultDetail: a jail running with
// desired stopped and no recorded detail (started outside Skep) is shown
// running with a detail saying it should be stopped.
func TestRunningAgainstDesiredStoppedDefaultDetail(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "outside", TemplateID: f.minimal})
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "stop"}, f.sub))
	f.fake.SetRunningJailForTest(c.ID)
	got, err := f.svc.Container(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != models.ContainerRunning || got.StateDetail != "the jail is running although it should be stopped; stop it again" {
		t.Fatalf("got %s %q", got.State, got.StateDetail)
	}
	// A running jail whose desired state is running has no detail.
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "stop"}, f.sub))
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	if got, _ := f.svc.Container(ctx, c.ID); got.State != models.ContainerRunning || got.StateDetail != "" {
		t.Fatalf("normal run: %s %q", got.State, got.StateDetail)
	}
}
