package bhyve_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/hostops/hostfake"
	"github.com/greenbeanorg/skep/internal/hypervisor/bhyve"
	"github.com/greenbeanorg/skep/internal/portfilter"
	"github.com/greenbeanorg/skep/pkg/models"
)

// TestRunnerFiltersTheTapBeforeTheBridge: the tap gets the VM program
// (MAC-only, IPv6 with the RA guard) before it joins the bridge, and a
// guest reboot keeps the tap and its filter (ADR 0011 decisions 3, 4).
func TestRunnerFiltersTheTapBeforeTheBridge(t *testing.T) {
	f := startRunner(t, spec())
	st := f.waitState(func(st hostops.RunState) bool { return st.Taps[0] != "" })
	tap := st.Taps[0]
	calls := f.fake.Calls()
	install := slices.Index(calls, "ngctl install filter on "+tap)
	addm := slices.Index(calls, "ifconfig bridge0 addm "+tap)
	if install < 0 || addm < 0 || install > addm {
		t.Fatalf("install at %d, addm at %d:\n%s", install, addm, strings.Join(calls, "\n"))
	}
	params, err := bhyve.VMFilterParams("02:00:00:00:00:01")
	if err != nil || !params.IPv6 || params.Static() {
		t.Fatalf("VM params = %+v, %v", params, err)
	}
	want, _ := portfilter.Program(params)
	if !portfilter.Equal(f.fake.FilterProgram(tap), want) {
		t.Fatal("the tap does not carry the VM program")
	}
	f.fake.Guest(vmID).Reboot()
	st = f.waitState(func(st hostops.RunState) bool { return st.Boots == 2 && st.State == models.VMRunning })
	if st.Taps[0] != tap || !f.fake.FilterInstalled(tap) {
		t.Errorf("after a reboot: tap %s (was %s), filtered %v", st.Taps[0], tap, f.fake.FilterInstalled(tap))
	}
	f.signals <- hostops.StopGraceful
	f.end()
}

// TestRunnerFailsWhenTheFilterFails: the VM never runs and the tap never
// joins the bridge.
func TestRunnerFailsWhenTheFilterFails(t *testing.T) {
	dir := t.TempDir()
	s := spec()
	s.Conf = hostops.Confinement{
		Pools: []string{"p"}, DatasetRoot: "skep", Bridges: []string{"bridge0"}, ManagementInterfaces: []string{"vtnet0"},
		FirmwareDir: dir + "/fw", RunDir: dir + "/run", StateDir: dir + "/state", MediaDir: dir + "/media",
	}
	fake := hostfake.New(s.Conf, hostfake.Options{})
	t.Cleanup(fake.Close)
	fake.FailPortFilters(true, false)
	ops := fake.Ops()
	if err := ops.Files.PrepareRun(context.Background(), vmID); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(s)
	if err := ops.Files.WriteRunFile(vmID, hostops.RunSpecFile, raw); err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := bhyve.RunVM(context.Background(), ops, vmID, 4242, make(chan hostops.RunnerSignal), quiet); err != nil {
		t.Fatal(err)
	}
	st, _, _ := hostops.ReadRunState(ops.Files, vmID)
	if st.State != models.VMFailed || !strings.Contains(st.Detail, models.CodePortFilterFailed) || st.Boots != 0 {
		t.Fatalf("run state = %+v", st)
	}
	for _, c := range fake.Calls() {
		if strings.Contains(c, "addm") {
			t.Fatalf("the tap joined the bridge: %q", c)
		}
	}
	if taps, _ := ops.Net.VMTaps(context.Background(), vmID); len(taps) != 0 {
		t.Errorf("taps left: %v", taps)
	}
}
