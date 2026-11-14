package bhyve_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/hostops/hostfake"
	"github.com/greenbeanorg/skep/internal/hypervisor/bhyve"
	"github.com/greenbeanorg/skep/pkg/models"
)

const vmID = "0b0b6f4e-5d0c-4a52-9a43-0d2f8a1f3c11"

func spec() bhyve.Spec {
	return bhyve.Spec{
		VMID: vmID, Name: "t", CPUs: 4, MemoryBytes: 2 << 30,
		Topology: &models.VMCPUTopology{Sockets: 1, Cores: 2, Threads: 2},
		Disks: []bhyve.SpecDisk{
			{Slot: 1, Bus: models.BusNVMe, Path: "/dev/zvol/p/skep/vm/" + vmID + "/disk1"},
			{Slot: 0, Bus: models.BusAHCIHD, Path: "/dev/zvol/p/skep/vm/" + vmID + "/disk0"},
			{Slot: 2, Bus: models.BusAHCICD, Path: "/media/x.iso", ReadOnly: true},
		},
		NICs: []bhyve.SpecNIC{{Slot: 0, Model: models.NICE1000, MAC: "02:00:00:00:00:01", Bridge: "bridge0"}},
	}
}

func TestRender(t *testing.T) {
	got, err := bhyve.Render(spec(), map[int]string{0: "tap7"}, "/fw/BHYVE_UEFI.fd", "/state/uefi-vars.fd")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"name=skep-" + vmID, "uuid=" + vmID, "cpus=4", "sockets=1", "cores=2", "threads=2",
		"memory.size=2048M", "acpi_tables=true", "rtc.use_localtime=false", "x86.vmexit_on_hlt=true",
		"x86.vmexit_on_pause=true", "x86.strictmsr=false", "bootrom=/fw/BHYVE_UEFI.fd",
		"bootvars=/state/uefi-vars.fd", "lpc.com1.path=/dev/nmdm-skep-" + vmID + ".1A",
		"pci.0.0.0.device=hostbridge", "pci.0.31.0.device=lpc",
		"pci.0.2.0.device=ahci", "pci.0.2.0.port.0.type=hd", "pci.0.2.0.port.0.path=/dev/zvol/p/skep/vm/" + vmID + "/disk0",
		"pci.0.3.0.device=nvme", "pci.0.3.0.path=/dev/zvol/p/skep/vm/" + vmID + "/disk1",
		"pci.0.4.0.device=ahci", "pci.0.4.0.port.0.type=cd", "pci.0.4.0.port.0.path=/media/x.iso", "pci.0.4.0.port.0.ro=true",
		"pci.0.10.0.device=e1000", "pci.0.10.0.backend=tap7", "pci.0.10.0.mac=02:00:00:00:00:01",
	}, "\n") + "\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	bad := spec()
	bad.Disks[0].Path = "/x\nbootrom=/evil"
	if _, err := bhyve.Render(bad, map[int]string{0: "tap7"}, "/fw", "/v"); err == nil {
		t.Error("a value with a line break was rendered")
	}
	if _, err := bhyve.Render(spec(), map[int]string{}, "/fw", "/v"); err == nil {
		t.Error("a NIC without a tap was rendered")
	}
	bad = spec()
	bad.Disks[0].Slot = 8
	if _, err := bhyve.Render(bad, map[int]string{0: "tap7"}, "/fw", "/v"); err == nil {
		t.Error("a disk slot colliding with the NICs was rendered")
	}
	dev, _ := bhyve.ConsoleDevice(vmID, 'B')
	if dev != "/dev/nmdm-skep-"+vmID+".1B" {
		t.Errorf("console device = %s", dev)
	}
}

// runFixture runs the runner loop against the fake, as the agent's
// development build does.
type runFixture struct {
	t       *testing.T
	fake    *hostfake.Fake
	ops     hostops.Ops
	signals chan hostops.RunnerSignal
	done    chan error
}

func startRunner(t *testing.T, s bhyve.Spec) *runFixture {
	t.Helper()
	dir := t.TempDir()
	conf := hostops.Confinement{
		Pools: []string{"p"}, DatasetRoot: "skep", Bridges: []string{"bridge0"}, ManagementInterfaces: []string{"vtnet0"},
		FirmwareDir: dir + "/fw", RunDir: dir + "/run", StateDir: dir + "/state", MediaDir: dir + "/media",
	}
	s.Conf = conf
	fake := hostfake.New(conf, hostfake.Options{})
	t.Cleanup(fake.Close)
	ops := fake.Ops()
	if err := ops.Files.PrepareRun(context.Background(), vmID); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(s)
	if err := ops.Files.WriteRunFile(vmID, hostops.RunSpecFile, raw); err != nil {
		t.Fatal(err)
	}
	f := &runFixture{t: t, fake: fake, ops: ops, signals: make(chan hostops.RunnerSignal, 2), done: make(chan error, 1)}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { f.done <- bhyve.RunVM(context.Background(), ops, vmID, 4242, f.signals, quiet) }()
	f.waitState(func(st hostops.RunState) bool { return st.State == models.VMRunning })
	return f
}

func (f *runFixture) waitState(cond func(hostops.RunState) bool) hostops.RunState {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, ok, _ := hostops.ReadRunState(f.ops.Files, vmID)
		if ok && cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("run state = %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *runFixture) end() hostops.RunState {
	f.t.Helper()
	select {
	case err := <-f.done:
		if err != nil {
			f.t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		f.t.Fatal("the runner did not end")
	}
	st, _, _ := hostops.ReadRunState(f.ops.Files, vmID)
	if !st.Final {
		f.t.Errorf("the last state is not final: %+v", st)
	}
	if f.fake.InstanceExists(vmID) || len(f.fake.Taps()) != 0 {
		f.t.Errorf("left behind: instance %v, taps %v", f.fake.InstanceExists(vmID), f.fake.Taps())
	}
	return st
}

func TestRunnerOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		do     func(*runFixture)
		state  string
		detail string
	}{
		{"guest power-off", func(f *runFixture) { f.fake.Guest(vmID).PowerOff() }, models.VMStopped, "powered off"},
		{"triple fault", func(f *runFixture) { f.fake.Guest(vmID).Crash() }, models.VMFailed, "triple"},
		{"graceful stop", func(f *runFixture) { f.signals <- hostops.StopGraceful }, models.VMStopped, ""},
		{"force stop", func(f *runFixture) { f.signals <- hostops.StopForce }, models.VMStopped, "force-stopped"},
		{"reboot, then stop", func(f *runFixture) {
			f.fake.Guest(vmID).Reboot()
			f.waitState(func(st hostops.RunState) bool { return st.Boots == 2 && st.State == models.VMRunning })
			f.signals <- hostops.StopGraceful
		}, models.VMStopped, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startRunner(t, spec())
			st := f.waitState(func(st hostops.RunState) bool { return st.Taps[0] != "" })
			if st.RunnerPID != 4242 || st.BhyvePID == 0 {
				t.Errorf("running state = %+v", st)
			}
			tc.do(f)
			st = f.end()
			if st.State != tc.state || !strings.Contains(st.Detail, tc.detail) {
				t.Errorf("final state = %+v, want %s containing %q", st, tc.state, tc.detail)
			}
		})
	}
}

func TestRunnerStopsARebootLoop(t *testing.T) {
	f := startRunner(t, spec())
	for i := 1; ; i++ {
		g := f.fake.Guest(vmID)
		if g == nil {
			break
		}
		g.Reboot()
		st := f.waitState(func(st hostops.RunState) bool { return st.Boots > i || st.Final })
		if st.Final {
			break
		}
	}
	if st := f.end(); st.State != models.VMFailed || !strings.Contains(st.Detail, "rebooted more than") {
		t.Errorf("final state = %+v", st)
	}
}
