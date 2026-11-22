package portcheck

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/hostops/hostfake"
	"github.com/greenbeanorg/skep/internal/portfilter"
)

const (
	jailID = "c0c0c0c0-1111-4222-8333-444455556666"
	vmID   = "0b0b6f4e-5d0c-4a52-9a43-0d2f8a1f3c11"
)

type auditLog struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (a *auditLog) Write(_ context.Context, e audit.Entry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
	return nil
}

func (a *auditLog) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, e := range a.entries {
		out = append(out, e.Action)
	}
	return out
}

// world is a fake host with a running jail on bridge0 (static NIC) and a
// running VM on bridge1, both filtered.
type world struct {
	fake       *hostfake.Fake
	checker    *Checker
	audit      *auditLog
	epair, tap string
	jail, vm   Port
}

func newWorld(t *testing.T) *world {
	t.Helper()
	conf := hostops.Confinement{Pools: []string{"devpool"}, DatasetRoot: "skep", Bridges: []string{"bridge0", "bridge1"}}
	fake := hostfake.New(conf, hostfake.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(fake.Close)
	w := &world{fake: fake, audit: &auditLog{}}
	ctx := context.Background()
	ops := fake.Ops()

	w.jail = Port{Kind: hostops.PortJail, OwnerID: jailID, Slot: 0, Bridge: "bridge0",
		Params: portfilter.Params{MAC: net.HardwareAddr{2, 0x5e, 0, 0, 0, 1}, IPv4: netip.MustParseAddr("192.0.2.10")}}
	w.epair = fake.LeaveEpair(jailID, 0) // a member of bridge0
	if _, err := ops.Filters.InstallPortFilter(ctx, w.epair, w.jail.Params); err != nil {
		t.Fatal(err)
	}
	w.vm = Port{Kind: hostops.PortVM, OwnerID: vmID, Slot: 0, Bridge: "bridge1",
		Params: portfilter.Params{MAC: net.HardwareAddr{2, 0x5e, 0, 0, 0, 2}, IPv6: true}}
	tap, err := ops.Net.CreateTap(ctx, vmID, 0)
	if err != nil {
		t.Fatal(err)
	}
	w.tap = tap
	if _, err := ops.Filters.InstallPortFilter(ctx, tap, w.vm.Params); err != nil {
		t.Fatal(err)
	}
	if err := ops.Net.AddBridgeMember(ctx, "bridge1", tap); err != nil {
		t.Fatal(err)
	}
	w.checker, err = New(Options{Host: ops, Audit: w.audit, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Ports: func(context.Context) ([]Port, error) { return []Port{w.jail, w.vm}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *world) check(t *testing.T) string {
	t.Helper()
	if err := w.checker.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	return w.checker.Probe(context.Background())
}

func TestHealthyPortsPass(t *testing.T) {
	w := newWorld(t)
	if p := w.check(t); p != "" {
		t.Fatalf("probe = %q", p)
	}
	if a := w.audit.actions(); len(a) != 0 {
		t.Errorf("audit entries for healthy ports: %v", a)
	}
}

func TestMissingFilterIsReinstalled(t *testing.T) {
	w := newWorld(t)
	w.fake.RemoveFilter(w.epair) // host root: ngctl rmhook epair: lower
	p := w.check(t)
	if !strings.Contains(p, w.epair) || !strings.Contains(p, "reinstalled") {
		t.Errorf("probe after the repair round = %q", p)
	}
	if !w.fake.FilterInstalled(w.epair) {
		t.Fatal("the filter was not reinstalled")
	}
	want, _ := portfilter.Program(w.jail.Params)
	if !portfilter.Equal(w.fake.FilterProgram(w.epair), want) {
		t.Error("the reinstalled program is not the port's")
	}
	if a := w.audit.actions(); strings.Join(a, ",") != "port_filter.missing,port_filter.restored" {
		t.Errorf("audit = %v", a)
	}
	if w.fake.PortBridge(w.epair) != "bridge0" {
		t.Error("the port was detached although the reinstall worked")
	}
	// The next round passes and clears the check.
	if p := w.check(t); p != "" {
		t.Errorf("probe after a clean round = %q", p)
	}
}

func TestReinstallFailureDetaches(t *testing.T) {
	w := newWorld(t)
	w.fake.RemoveFilter(w.tap)
	w.fake.FailPortFilters(false, true)
	p := w.check(t)
	if w.fake.PortBridge(w.tap) != "" {
		t.Fatal("a port whose filter could not be reinstalled is still on its bridge")
	}
	if !strings.Contains(p, "detached") {
		t.Errorf("probe = %q", p)
	}
	if a := w.audit.actions(); strings.Join(a, ",") != "port_filter.missing,port_filter.detached" {
		t.Errorf("audit = %v", a)
	}
	// It stays reported, and nothing more is tried, until the guest is
	// restarted; the healthy jail port is not affected.
	w.fake.FailPortFilters(false, false)
	if p := w.check(t); !strings.Contains(p, "detached") || strings.Contains(p, w.epair) {
		t.Errorf("probe on the next round = %q", p)
	}
	if n := len(w.audit.actions()); n != 2 {
		t.Errorf("%d audit entries after the next round, want 2", n)
	}
}

func TestThreeFailuresInARowDetach(t *testing.T) {
	w := newWorld(t)
	for i := 1; i <= MaxConsecutiveFailures; i++ {
		w.fake.RemoveFilter(w.epair) // removed again after every repair
		w.check(t)
		member := w.fake.PortBridge(w.epair) == "bridge0"
		if i < MaxConsecutiveFailures && !member {
			t.Fatalf("detached after %d failures", i)
		}
		if i == MaxConsecutiveFailures && member {
			t.Fatalf("still on the bridge after %d failures in a row", i)
		}
	}
	want := "port_filter.missing,port_filter.restored,port_filter.missing,port_filter.restored,port_filter.missing,port_filter.detached"
	if a := strings.Join(w.audit.actions(), ","); a != want {
		t.Errorf("audit = %s", a)
	}
}

func TestAPassResetsTheCount(t *testing.T) {
	w := newWorld(t)
	for i := 0; i < 2*MaxConsecutiveFailures; i++ {
		w.fake.RemoveFilter(w.epair)
		w.check(t) // repaired
		w.check(t) // passes: the count starts again
	}
	if w.fake.PortBridge(w.epair) != "bridge0" {
		t.Fatal("failures that were not in a row detached the port")
	}
}

// TestLockedPortIsSkipped: a port whose lock is held (an install running,
// or a tap being recreated) is never repaired concurrently.
func TestLockedPortIsSkipped(t *testing.T) {
	w := newWorld(t)
	w.fake.RemoveFilter(w.tap)
	unlock, err := w.fake.Ops().Filters.LockPort(hostops.PortVM, vmID, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	w.check(t)
	if w.fake.FilterInstalled(w.tap) || len(w.audit.actions()) != 0 {
		t.Fatal("a locked port was repaired")
	}
	unlock()
	w.check(t)
	if !w.fake.FilterInstalled(w.tap) {
		t.Fatal("not repaired once the lock was free")
	}
}

// TestPortOfAnotherOwnerIsSkipped: the interface named for a port now
// carries another owner's description (its name was reused).
func TestPortOfAnotherOwnerIsSkipped(t *testing.T) {
	w := newWorld(t)
	w.jail.OwnerID = "d1d1d1d1-2222-4333-8444-555566667777"
	w.fake.RemoveFilter(w.epair)
	w.check(t)
	if w.fake.FilterInstalled(w.epair) || len(w.audit.actions()) != 0 {
		t.Fatal("another owner's port was touched")
	}
}

func TestModulesMissing(t *testing.T) {
	w := newWorld(t)
	w.fake.NoNetgraph = true
	w.fake.RemoveFilter(w.epair)
	p := w.check(t)
	if !strings.Contains(p, "netgraph") {
		t.Errorf("probe = %q", p)
	}
	if w.fake.FilterInstalled(w.epair) || w.fake.PortBridge(w.epair) != "bridge0" || len(w.audit.actions()) != 0 {
		t.Error("something was installed or detached without the modules")
	}
}

func TestDetachedPortIsReportedAfterARestart(t *testing.T) {
	w := newWorld(t)
	if err := w.fake.Ops().Filters.DetachPort(context.Background(), "bridge1", w.tap); err != nil {
		t.Fatal(err)
	}
	// A new checker, as after an agent restart, knows nothing of it.
	c, _ := New(w.checker.opts)
	if err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := c.Probe(context.Background()); !strings.Contains(p, "detached") {
		t.Errorf("probe = %q", p)
	}
}

func TestVLANProbe(t *testing.T) {
	w := newWorld(t)
	w.check(t)
	if p := w.checker.VLANProbe(context.Background()); !strings.Contains(p, "bridge0 has no VLAN filtering") || !strings.Contains(p, "bridge1") {
		t.Errorf("unfiltered bridges: %q", p)
	}
	w.fake.SetBridgeVLAN("bridge0", true, map[string]int{w.epair: 1})
	w.fake.SetBridgeVLAN("bridge1", true, map[string]int{})
	w.check(t)
	p := w.checker.VLANProbe(context.Background())
	if strings.Contains(p, "bridge0") || !strings.Contains(p, "bridge1: guest port(s) without a PVID: "+w.tap) {
		t.Errorf("probe = %q", p)
	}
	w.fake.SetBridgeVLAN("bridge1", true, map[string]int{w.tap: 1})
	w.check(t)
	if p := w.checker.VLANProbe(context.Background()); p != "" {
		t.Errorf("filtered bridges: %q", p)
	}
	// The VLAN warning never touches the port filters.
	if p := w.checker.Probe(context.Background()); p != "" {
		t.Errorf("port_filters = %q", p)
	}
}
