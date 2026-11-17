package orphans

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/hostops/hostfake"
)

const (
	running  = "a0a0a0a0-1111-4222-8333-444455556666"
	stopped  = "b0b0b0b0-1111-4222-8333-444455556666"
	starting = "c0c0c0c0-1111-4222-8333-444455556666"
	gone     = "d0d0d0d0-1111-4222-8333-444455556666"
	vmUp     = "e0e0e0e0-1111-4222-8333-444455556666"
	vmDown   = "f0f0f0f0-1111-4222-8333-444455556666"
)

func TestFindReportProbe(t *testing.T) {
	ctx := context.Background()
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	conf := hostops.Confinement{Pools: []string{"devpool"}, DatasetRoot: "skep", Bridges: []string{"bridge0"}}
	fake := hostfake.New(conf, hostfake.Options{Logger: log})
	t.Cleanup(fake.Close)
	ops := fake.Ops()

	// A running jail with its epair, and leftovers of three others.
	fake.LeaveEpair(running, 0)
	_ = fake.SetRunningJailForTest(running)
	fake.LeaveEpair(stopped, 0)
	fake.LeaveEpair(starting, 1)
	fake.LeaveEpair(gone, 2)
	// Two VM taps: one VM's instance exists.
	tapUp, _ := ops.Net.CreateTap(ctx, vmUp, 0)
	tapDown, _ := ops.Net.CreateTap(ctx, vmDown, 0)
	fake.SetInstance(vmUp, true)

	owners := map[string]Owner{running: {Exists: true}, stopped: {Exists: true}, starting: {Exists: true, Busy: true},
		vmUp: {Exists: true}, vmDown: {Exists: true}}
	lookup := func(_ context.Context, id string) (Owner, error) { return owners[id], nil }
	now := time.Unix(1000, 0)
	c, err := New(Options{Jails: ops.Jails, VMM: ops.VMM, Container: lookup, VM: lookup, Logger: log, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	found, err := c.Find(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Orphan{}
	for _, o := range found {
		got[o.OwnerID] = o
	}
	if len(found) != 3 || !got[stopped].OwnerExists || got[gone].OwnerExists || got[gone].Slot != 2 ||
		got[vmDown].Interface != tapDown || got[vmDown].Kind != "tap" {
		t.Fatalf("Find = %+v (tap of the running VM: %s)", found, tapUp)
	}

	if err := c.Report(ctx); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs.String(), `"event":"orphaned_interface"`); n != 3 {
		t.Fatalf("%d warning events:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), `"event":"orphaned_interface_check","found":3`) {
		t.Fatalf("no summary line:\n%s", logs.String())
	}

	// The probe names no interface and is cached.
	p := c.Probe(ctx)
	if !strings.HasPrefix(p, "3 interface(s)") || strings.Contains(p, "epair") || strings.Contains(p, stopped) {
		t.Fatalf("Probe = %q", p)
	}
	for id := range map[string]bool{stopped: true, gone: true} {
		_, _ = ops.Jails.DestroyJailInterfaces(ctx, id)
	}
	_ = ops.Net.DestroyTap(ctx, vmDown, tapDown)
	if c.Probe(ctx) != p {
		t.Fatal("the probe was not cached")
	}
	now = now.Add(DefaultCacheTTL)
	if p := c.Probe(ctx); p != "" {
		t.Fatalf("after the clean-up: %q", p)
	}
}
