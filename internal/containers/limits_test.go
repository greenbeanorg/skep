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
