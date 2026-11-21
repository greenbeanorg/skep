package containers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/hostops/hostfake"
	"github.com/greenbeanorg/skep/pkg/models"
)

func (f *fixture) called(want string) bool {
	return strings.Contains(strings.Join(f.fake.Calls(), "\n"), want)
}

func TestLimitsReachTheJail(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	limits := models.ContainerLimits{Memory: 256 << 20, CPUPercent: 50, MaxProcesses: 200, OpenFiles: 4096, DiskQuota: 1 << 30,
		LockedMemory: 32 << 20}
	c := f.create(models.ContainerCreate{Name: "lim", TemplateID: f.minimal, Limits: &limits,
		Options: &models.ContainerOptions{Mlock: true}})
	if c.Limits != limits || !c.Options.Mlock {
		t.Fatalf("stored = %+v %+v", c.Limits, c.Options)
	}
	if !f.called("zfs set refquota=1073741824 devpool/skep/jail/" + c.ID) {
		t.Fatal("the quota was not set at create")
	}
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	j := "jail:skep-" + c.ID + ":"
	want := []string{j + "memoryuse:deny=268435456", j + "pcpu:deny=50", j + "maxproc:deny=200", j + "openfiles:deny=4096",
		j + "memorylocked:deny=33554432"}
	if got := f.fake.RctlRules(c.ID); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("rules = %q", got)
	}
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "stop"}, f.sub))
	if got := f.fake.RctlRules(c.ID); len(got) != 0 {
		t.Fatalf("rules after stop = %q", got)
	}

	// An update replaces the limits: the quota follows, and mlock without
	// the locked-memory limit is refused, even with a memory limit.
	_, err := f.svc.UpdateContainer(ctx, c.ID, models.ContainerUpdate{Limits: &models.ContainerLimits{CPUPercent: 25, Memory: 256 << 20}}, f.sub)
	wantCode(t, err, models.CodeMlockRequiresMemoryLimit)
	f.succeed(f.svc.UpdateContainer(ctx, c.ID, models.ContainerUpdate{Limits: &models.ContainerLimits{CPUPercent: 25},
		Options: &models.ContainerOptions{}}, f.sub))
	if !f.called("zfs set refquota=0 devpool/skep/jail/" + c.ID) {
		t.Fatal("removing the quota did not reach the dataset")
	}
	c, _ = f.svc.Container(ctx, c.ID)
	if c.Limits != (models.ContainerLimits{CPUPercent: 25}) || c.Options.Mlock {
		t.Fatalf("after update = %+v %+v", c.Limits, c.Options)
	}
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	if got := f.fake.RctlRules(c.ID); strings.Join(got, " ") != j+"pcpu:deny=25" {
		t.Fatalf("rules after update = %q", got)
	}
}

func TestLimitRefusals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for name, c := range map[string]struct {
		limits models.ContainerLimits
		opts   models.ContainerOptions
		code   string
	}{
		"negative":           {models.ContainerLimits{MaxProcesses: -1}, models.ContainerOptions{}, models.CodeInvalidRequest},
		"memory too small":   {models.ContainerLimits{Memory: 1 << 20}, models.ContainerOptions{}, models.CodeInvalidRequest},
		"cpu past the host":  {models.ContainerLimits{CPUPercent: 401}, models.ContainerOptions{}, models.CodeInvalidRequest},
		"too few files":      {models.ContainerLimits{OpenFiles: 10}, models.ContainerOptions{}, models.CodeInvalidRequest},
		"quota too small":    {models.ContainerLimits{DiskQuota: 1 << 20}, models.ContainerOptions{}, models.CodeInvalidRequest},
		"mlock, no memory":   {models.ContainerLimits{CPUPercent: 50}, models.ContainerOptions{Mlock: true}, models.CodeMlockRequiresMemoryLimit},
		"mlock, only quota":  {models.ContainerLimits{DiskQuota: 1 << 30}, models.ContainerOptions{Mlock: true}, models.CodeMlockRequiresMemoryLimit},
		"mlock, memory only": {models.ContainerLimits{Memory: 256 << 20}, models.ContainerOptions{Mlock: true}, models.CodeMlockRequiresMemoryLimit},
		"locked over memory": {models.ContainerLimits{Memory: 64 << 20, LockedMemory: 128 << 20}, models.ContainerOptions{Mlock: true}, models.CodeInvalidRequest},
		"negative locked":    {models.ContainerLimits{LockedMemory: -1}, models.ContainerOptions{}, models.CodeInvalidRequest},
	} {
		l, o := c.limits, c.opts
		_, err := f.svc.CreateContainer(ctx, models.ContainerCreate{Name: "w", TemplateID: f.minimal, Limits: &l, Options: &o}, f.sub)
		wantCodeNamed(t, name, err, c.code)
	}
	// 400 is four CPUs, the fake's count.
	f.create(models.ContainerCreate{Name: "all-cpus", TemplateID: f.minimal, Limits: &models.ContainerLimits{CPUPercent: 400}})
}

func wantCodeNamed(t *testing.T, name string, err error, code string) {
	t.Helper()
	var me *models.Error
	if !errors.As(err, &me) || me.Code != code {
		t.Errorf("%s: %v, want %s", name, err, code)
	}
}

func TestRacctDisabled(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// Made while RACCT was on, with a CPU limit and a quota.
	c := f.create(models.ContainerCreate{Name: "lim", TemplateID: f.minimal,
		Limits: &models.ContainerLimits{CPUPercent: 50, DiskQuota: 1 << 30}})
	plain := f.create(models.ContainerCreate{Name: "plain", TemplateID: f.minimal})
	// The host reboots without kern.racct.enable.
	f.fake.Set(func(h *hostfake.Fake) { h.RacctDisabled = true })

	_, err := f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub)
	wantCode(t, err, models.CodeRctlUnavailable)
	if f.called("jail -c skep-" + c.ID) {
		t.Fatal("a jail with limits started without RACCT")
	}
	_, err = f.svc.CreateContainer(ctx, models.ContainerCreate{Name: "w", TemplateID: f.minimal,
		Limits: &models.ContainerLimits{Memory: 256 << 20}}, f.sub)
	wantCode(t, err, models.CodeRctlUnavailable)
	_, err = f.svc.UpdateContainer(ctx, plain.ID, models.ContainerUpdate{Limits: &models.ContainerLimits{MaxProcesses: 10}}, f.sub)
	wantCode(t, err, models.CodeRctlUnavailable)
	// mlock with a locked-memory limit needs RACCT too: the limit is
	// refused first.
	_, err = f.svc.CreateContainer(ctx, models.ContainerCreate{Name: "w", TemplateID: f.minimal,
		Limits: &models.ContainerLimits{LockedMemory: 16 << 20}, Options: &models.ContainerOptions{Mlock: true}}, f.sub)
	wantCode(t, err, models.CodeRctlUnavailable)

	// A disk quota works without RACCT, and so does a jail without
	// limits.
	q := f.create(models.ContainerCreate{Name: "quota", TemplateID: f.minimal, Limits: &models.ContainerLimits{DiskQuota: 2 << 30}})
	f.succeed(f.svc.Action(ctx, q.ID, models.ContainerAction{Action: "start"}, f.sub))
	f.succeed(f.svc.Action(ctx, plain.ID, models.ContainerAction{Action: "start"}, f.sub))
	// Removing the limits lets the first one start.
	f.succeed(f.svc.UpdateContainer(ctx, c.ID, models.ContainerUpdate{Limits: &models.ContainerLimits{DiskQuota: 1 << 30}}, f.sub))
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
}

// The host's total of locked memory (amendment o, SR-04): a quarter of
// physical memory unless containers.max_locked_memory_bytes is set.
func TestMaxLockedMemoryDefault(t *testing.T) {
	for name, c := range map[string]struct{ configured, physical, want int64 }{
		"a quarter of physical memory": {0, 16 << 30, 4 << 30},
		"hearst's 6 GiB":               {0, 6 << 30, 1536 << 20},
		"rounded down":                 {0, 1<<30 + 3, 1 << 28},
		"configured wins":              {512 << 20, 16 << 30, 512 << 20},
		"configured above physical":    {32 << 30, 16 << 30, 32 << 30},
		"unreadable physical memory":   {0, 0, 0},
		"negative physical memory":     {0, -1, 0},
	} {
		if got := MaxLockedMemory(c.configured, c.physical); got != c.want {
			t.Errorf("%s: MaxLockedMemory(%d, %d) = %d, want %d", name, c.configured, c.physical, got, c.want)
		}
	}
	// New takes the default from the physical memory it is given.
	f := newFixture(t)
	if f.svc.maxLocked != 4<<30 {
		t.Fatalf("the fixture's total = %d, want a quarter of 16 GiB", f.svc.maxLocked)
	}
}

// A single locked_memory above the total is refused with 422 at create
// and update; a value at the total is accepted.
func TestLockedMemoryAboveTheTotal(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.MaxLockedMemory = 64 << 20 })
	ctx := context.Background()
	mlock := &models.ContainerOptions{Mlock: true}
	_, err := f.svc.CreateContainer(ctx, models.ContainerCreate{Name: "big", TemplateID: f.minimal,
		Limits: &models.ContainerLimits{LockedMemory: 64<<20 + 1}, Options: mlock}, f.sub)
	wantCode(t, err, models.CodeContainerLockedMemoryAboveMax)
	var me *models.Error
	errors.As(err, &me)
	if me.Details["max_locked_memory_bytes"] != int64(64<<20) {
		t.Fatalf("details = %v", me.Details)
	}
	// Without mlock too: the limit itself is bounded.
	_, err = f.svc.CreateContainer(ctx, models.ContainerCreate{Name: "big", TemplateID: f.minimal,
		Limits: &models.ContainerLimits{LockedMemory: 1 << 40}}, f.sub)
	wantCode(t, err, models.CodeContainerLockedMemoryAboveMax)
	c := f.create(models.ContainerCreate{Name: "at", TemplateID: f.minimal,
		Limits: &models.ContainerLimits{LockedMemory: 64 << 20}, Options: mlock})
	_, err = f.svc.UpdateContainer(ctx, c.ID, models.ContainerUpdate{Limits: &models.ContainerLimits{LockedMemory: 1 << 30}}, f.sub)
	wantCode(t, err, models.CodeContainerLockedMemoryAboveMax)
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))

	// No physical memory read and nothing configured: no jail may lock.
	g := newFixture(t, func(o *Options) { o.PhysicalMemory = 0 })
	_, err = g.svc.CreateContainer(ctx, models.ContainerCreate{Name: "any", TemplateID: g.minimal,
		Limits: &models.ContainerLimits{LockedMemory: 1 << 20}, Options: mlock}, g.sub)
	wantCode(t, err, models.CodeContainerLockedMemoryAboveMax)
	g.create(models.ContainerCreate{Name: "none", TemplateID: g.minimal, Limits: &models.ContainerLimits{CPUPercent: 50}})
}

// At start, this jail's locked_memory plus that of the running jails
// must fit in the total; the jail itself is never counted twice, and a
// jail without locked memory always fits.
func TestLockedMemoryTotalAtStart(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.MaxLockedMemory = 100 << 20 })
	ctx := context.Background()
	mlock := &models.ContainerOptions{Mlock: true}
	a := f.create(models.ContainerCreate{Name: "a", TemplateID: f.minimal, Limits: &models.ContainerLimits{LockedMemory: 60 << 20}, Options: mlock})
	b := f.create(models.ContainerCreate{Name: "b", TemplateID: f.minimal, Limits: &models.ContainerLimits{LockedMemory: 50 << 20}})
	c := f.create(models.ContainerCreate{Name: "c", TemplateID: f.minimal, Limits: &models.ContainerLimits{LockedMemory: 40 << 20}})
	plain := f.create(models.ContainerCreate{Name: "plain", TemplateID: f.minimal})

	f.succeed(f.svc.Action(ctx, a.ID, models.ContainerAction{Action: "start"}, f.sub))
	// A jail the host reports running is not counted against itself.
	if err := f.svc.checkLockedMemoryTotal(ctx, a); err != nil {
		t.Fatalf("a running jail was counted against itself: %v", err)
	}
	_, err := f.svc.Action(ctx, b.ID, models.ContainerAction{Action: "start"}, f.sub)
	wantCode(t, err, models.CodeContainerLockedMemoryExhausted)
	var me *models.Error
	errors.As(err, &me)
	if me.Details["running_bytes"] != int64(60<<20) || me.Details["required_bytes"] != int64(50<<20) {
		t.Fatalf("details = %v", me.Details)
	}
	if f.called("jail -c skep-" + b.ID) {
		t.Fatal("a refused jail was created")
	}
	// 60 + 40 is exactly the total.
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	f.succeed(f.svc.Action(ctx, plain.ID, models.ContainerAction{Action: "start"}, f.sub))
	// A restart (a stop, then a start with its own check) fits.
	f.succeed(f.svc.Action(ctx, a.ID, models.ContainerAction{Action: "restart"}, f.sub))
	// Stopping a frees its share.
	f.succeed(f.svc.Action(ctx, a.ID, models.ContainerAction{Action: "stop"}, f.sub))
	f.succeed(f.svc.Action(ctx, b.ID, models.ContainerAction{Action: "start"}, f.sub))
	_, err = f.svc.Action(ctx, a.ID, models.ContainerAction{Action: "start"}, f.sub)
	wantCode(t, err, models.CodeContainerLockedMemoryExhausted)
}

// A start under way holds its share: a second start checked while the
// first is reserved is refused (the task-time check, which the
// submission check cannot see), and the share returns on release.
func TestLockedMemoryReservation(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.MaxLockedMemory = 100 << 20 })
	ctx := context.Background()
	a := f.create(models.ContainerCreate{Name: "a", TemplateID: f.minimal, Limits: &models.ContainerLimits{LockedMemory: 60 << 20}})
	b := f.create(models.ContainerCreate{Name: "b", TemplateID: f.minimal, Limits: &models.ContainerLimits{LockedMemory: 60 << 20}})
	release, err := f.svc.reserveLockedMemory(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.reserveLockedMemory(ctx, b); err == nil {
		t.Fatal("a second start took the share of a start under way")
	} else {
		wantCode(t, err, models.CodeContainerLockedMemoryExhausted)
	}
	wantCode(t, f.svc.checkLockedMemoryTotal(ctx, b), models.CodeContainerLockedMemoryExhausted)
	// The same jail is not counted against itself.
	if err := f.svc.checkLockedMemoryTotal(ctx, a); err != nil {
		t.Fatal(err)
	}
	release()
	releaseB, err := f.svc.reserveLockedMemory(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	releaseB()
	if len(f.svc.lockedStarting) != 0 {
		t.Fatalf("reservations left: %v", f.svc.lockedStarting)
	}
}

// A disk quota below what the jail's root already references is refused
// at submission with container_disk_quota_below_usage, before any task
// exists (amendment w); one at or above it is accepted and set. The fake
// template, and so a new jail's root, references 132 MiB.
func TestUpdateQuotaBelowUsage(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "q", TemplateID: f.minimal, Limits: &models.ContainerLimits{DiskQuota: 1 << 30}})
	const referenced = int64(132 << 20)
	tasksBefore := f.count(`SELECT COUNT(*) FROM tasks WHERE type = ?`, models.TaskContainerUpdate)

	// Above the 64 MiB minimum, below what the root references.
	_, err := f.svc.UpdateContainer(ctx, c.ID, models.ContainerUpdate{Limits: &models.ContainerLimits{DiskQuota: 100 << 20}}, f.sub)
	wantCode(t, err, models.CodeContainerDiskQuotaBelowUsage)
	var me *models.Error
	errors.As(err, &me)
	if me.Details["referenced_bytes"] != referenced || me.Details["disk_quota"] != int64(100<<20) || me.Details["container_id"] != c.ID {
		t.Fatalf("details = %v", me.Details)
	}
	// One byte short is refused too, with other changes in the request.
	host := "renamed"
	_, err = f.svc.UpdateContainer(ctx, c.ID, models.ContainerUpdate{Hostname: &host,
		Limits: &models.ContainerLimits{DiskQuota: models.ByteSize(referenced - 1)}}, f.sub)
	wantCode(t, err, models.CodeContainerDiskQuotaBelowUsage)
	// No task was submitted; each refusal is audited as denied; nothing
	// changed.
	if n := f.count(`SELECT COUNT(*) FROM tasks WHERE type = ?`, models.TaskContainerUpdate); n != tasksBefore {
		t.Fatalf("%d update tasks, want %d", n, tasksBefore)
	}
	if n := f.count(`SELECT COUNT(*) FROM audit_log WHERE outcome = 'denied' AND resource_id = ? AND details LIKE '%' || ? || '%'`,
		c.ID, models.CodeContainerDiskQuotaBelowUsage); n != 2 {
		t.Fatalf("%d denied audit entries, want 2", n)
	}
	for _, call := range f.fake.Calls() {
		if strings.HasPrefix(call, "zfs set refquota=") && !strings.HasPrefix(call, "zfs set refquota=1073741824 ") {
			t.Fatalf("a refused quota reached the host: %s", call)
		}
	}
	if got, err := f.svc.Container(ctx, c.ID); err != nil || got.Limits.DiskQuota != 1<<30 || got.Hostname != "q" {
		t.Fatalf("after the refusals: %+v %v", got, err)
	}

	// Exactly what the root references is accepted and set.
	f.succeed(f.svc.UpdateContainer(ctx, c.ID, models.ContainerUpdate{Limits: &models.ContainerLimits{DiskQuota: models.ByteSize(referenced)}}, f.sub))
	if got, _ := f.svc.Container(ctx, c.ID); got.Limits.DiskQuota != models.ByteSize(referenced) {
		t.Fatalf("quota = %d, want %d", got.Limits.DiskQuota, referenced)
	}
	// Removing the quota needs no check.
	f.succeed(f.svc.UpdateContainer(ctx, c.ID, models.ContainerUpdate{Limits: &models.ContainerLimits{}}, f.sub))
}
