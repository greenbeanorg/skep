package vm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/hostops/hostfake"
	"github.com/greenbeanorg/skep/pkg/models"
)

// act submits an action and waits for its task.
func (f *fixture) act(id, action string, timeout *int) (models.Task, error) {
	f.t.Helper()
	task, err := f.svc.Action(context.Background(), id, models.VMAction{Action: action, Timeout: timeout}, f.sub)
	if err != nil {
		return models.Task{}, err
	}
	done := f.wait(task.ID)
	return done.Public(), nil
}

func (f *fixture) observed(id string) models.VM {
	f.t.Helper()
	if _, err := f.svc.Observe(context.Background(), id); err != nil {
		f.t.Fatal(err)
	}
	vm, err := f.svc.VM(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return vm
}

func (f *fixture) eventually(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartStopLifecycle(t *testing.T) {
	f := newFixture(t)
	vm := f.create(basicVM("web1"))

	task, err := f.act(vm.ID, models.VMActionStart, nil)
	if err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("start: %v %+v", err, task)
	}
	got := f.observed(vm.ID)
	if got.Observed.State != models.VMRunning || got.DesiredState != models.VMRunning || got.Observed.PID == nil {
		t.Fatalf("after start: %+v", got)
	}
	if got.NICs[0].Tap == "" {
		t.Errorf("no tap recorded: %+v", got.NICs)
	}
	taps := f.fake.Taps()
	if len(taps) != 1 || !strings.HasSuffix(taps[0], " bridge0") {
		t.Errorf("taps = %v", taps)
	}
	// The rendered configuration is kept for diagnosis and names the
	// zvol, the ISO, the tap, and the console.
	path, _ := f.fake.Ops().Files.StateFilePath(vm.ID, hostops.BhyveConfigFile)
	conf := readFile(t, path)
	for _, want := range []string{
		"name=skep-" + vm.ID, "uuid=" + vm.ID, "cpus=2", "memory.size=1024M",
		"pci.0.2.0.device=virtio-blk", "pci.0.2.0.path=/dev/zvol/devpool/skep/vm/" + vm.ID + "/disk0",
		"pci.0.3.0.port.0.type=cd", "pci.0.3.0.port.0.ro=true",
		"pci.0.10.0.device=virtio-net", "pci.0.10.0.backend=tap0", "pci.0.10.0.mac=" + vm.NICs[0].MACAddress,
		"lpc.com1.path=/dev/nmdm-skep-" + vm.ID + ".1A",
	} {
		if !strings.Contains(conf, want+"\n") {
			t.Errorf("bhyve.conf lacks %q:\n%s", want, conf)
		}
	}

	// A second start is refused before a task exists.
	if _, err := f.svc.Action(context.Background(), vm.ID, models.VMAction{Action: "start"}, f.sub); code(err) != models.CodeVMNotStopped {
		t.Errorf("second start: %v", err)
	}
	// So are update and delete of a running VM.
	if _, err := f.svc.DeleteVM(context.Background(), vm.ID, f.sub); code(err) != models.CodeVMNotStopped {
		t.Errorf("delete while running: %v", err)
	}

	// The guest reboots: the runner starts bhyve again by itself.
	f.fake.Guest(vm.ID).Reboot()
	f.eventually("the second boot", func() bool {
		st, _, _ := hostops.ReadRunState(f.fake.Ops().Files, vm.ID)
		return st.Boots == 2 && st.State == models.VMRunning
	})

	task, err = f.act(vm.ID, models.VMActionStop, nil)
	if err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("stop: %v %+v", err, task)
	}
	got = f.observed(vm.ID)
	if got.Observed.State != models.VMStopped || got.DesiredState != models.VMStopped || got.Drift {
		t.Fatalf("after stop: %+v", got)
	}
	if taps := f.fake.Taps(); len(taps) != 0 {
		t.Errorf("taps after stop = %v", taps)
	}
	if f.fake.InstanceExists(vm.ID) {
		t.Error("the instance survived the stop")
	}
	// Stopping a stopped VM is refused.
	if _, err := f.svc.Action(context.Background(), vm.ID, models.VMAction{Action: "stop"}, f.sub); code(err) != models.CodeVMNotRunning {
		t.Errorf("stop of a stopped VM: %v", err)
	}
	// Both phases of each action are in the agent's audit log, and so
	// are the two refusals.
	for _, tc := range []struct {
		phase, outcome string
		want           int
	}{{"requested", "success", 2}, {"finished", "success", 2}, {"requested", "denied", 2}} {
		if n := f.count(`SELECT COUNT(*) FROM audit_log WHERE action IN ('vm.start','vm.stop') AND resource_id = ?
			AND phase = ? AND outcome = ?`, vm.ID, tc.phase, tc.outcome); n != tc.want {
			t.Errorf("%d %s/%s start and stop entries, want %d", n, tc.phase, tc.outcome, tc.want)
		}
	}
}

func TestStopNeverEscalates(t *testing.T) {
	f := newFixture(t)
	vm := f.create(basicVM("menu"))
	if task, err := f.act(vm.ID, models.VMActionStart, nil); err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("start: %v %+v", err, task)
	}
	f.fake.Set(func(fk *hostfake.Fake) { fk.IgnoreACPI[vm.ID] = true })
	one := 1
	task, err := f.act(vm.ID, models.VMActionStop, &one)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != models.TaskFailed || task.Error == nil || task.Error.Code != models.CodeStopTimeout {
		t.Fatalf("stop of a guest that ignores ACPI: %+v", task)
	}
	if got := f.observed(vm.ID); got.Observed.State != models.VMStopping || !f.fake.InstanceExists(vm.ID) {
		t.Fatalf("the VM did not keep running: %+v", got.Observed)
	}

	// Force stop ends it.
	task, err = f.act(vm.ID, models.VMActionForceStop, nil)
	if err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("force stop: %v %+v", err, task)
	}
	if got := f.observed(vm.ID); got.Observed.State != models.VMStopped || f.fake.InstanceExists(vm.ID) || len(f.fake.Taps()) != 0 {
		t.Fatalf("after force stop: %+v, taps %v", got.Observed, f.fake.Taps())
	}
}

func TestGuestPowerOffAndCrash(t *testing.T) {
	f := newFixture(t)
	vm := f.create(basicVM("g"))
	f.act(vm.ID, models.VMActionStart, nil)
	f.fake.Guest(vm.ID).PowerOff()
	f.eventually("stopped", func() bool { return f.observed(vm.ID).Observed.State == models.VMStopped })
	got := f.observed(vm.ID)
	// The guest's own power-off leaves the desired state alone: drift is
	// shown, and nothing restarts the VM.
	if got.DesiredState != models.VMRunning || !got.Drift {
		t.Errorf("after the guest powered off: %+v", got)
	}

	f.act(vm.ID, models.VMActionStart, nil)
	f.fake.Guest(vm.ID).Crash()
	f.eventually("failed", func() bool { return f.observed(vm.ID).Observed.State == models.VMFailed })
	if got := f.observed(vm.ID); !strings.Contains(got.Observed.Detail, "triple") {
		t.Errorf("detail = %q", got.Observed.Detail)
	}
	// A failed run is over: the VM can start again.
	if task, _ := f.act(vm.ID, models.VMActionStart, nil); task.Status != models.TaskSucceeded {
		t.Errorf("start after a crash: %+v", task)
	}
}

func TestKilledRunnerLeavesAnInstanceForceStopDestroys(t *testing.T) {
	f := newFixture(t)
	vm := f.create(basicVM("orphan"))
	f.act(vm.ID, models.VMActionStart, nil)
	f.fake.KillRunner(vm.ID)
	got := f.observed(vm.ID)
	if got.Observed.State != models.VMFailed || !strings.Contains(got.Observed.Detail, "without a runner") {
		t.Fatalf("after the runner died: %+v", got.Observed)
	}
	// A start, a stop, and a delete are refused; force stop is the way out.
	if _, err := f.svc.Action(context.Background(), vm.ID, models.VMAction{Action: "start"}, f.sub); code(err) != models.CodeVMNotStopped {
		t.Errorf("start: %v", err)
	}
	if task, _ := f.act(vm.ID, models.VMActionStop, nil); task.Status != models.TaskFailed || task.Error.Code != models.CodeVMNotRunning {
		t.Errorf("stop: %+v", task)
	}
	if task, _ := f.act(vm.ID, models.VMActionForceStop, nil); task.Status != models.TaskSucceeded {
		t.Fatalf("force stop: %+v", task)
	}
	if f.fake.InstanceExists(vm.ID) || len(f.fake.Taps()) != 0 {
		t.Errorf("left behind: instance %v, taps %v", f.fake.InstanceExists(vm.ID), f.fake.Taps())
	}
	if got := f.observed(vm.ID); got.Observed.State != models.VMStopped {
		t.Errorf("after force stop: %+v", got.Observed)
	}
}

func TestStartRefusals(t *testing.T) {
	f := newFixture(t)
	big := basicVM("big")
	big.Memory = models.ByteSize(3 * gib)
	a := f.create(big)
	big.Name = "big2"
	b := f.create(big)
	if task, _ := f.act(a.ID, models.VMActionStart, nil); task.Status != models.TaskSucceeded {
		t.Fatalf("start a: %+v", task)
	}
	f.observed(a.ID)
	// 6 GiB less the 1 GiB reserve leaves 5 GiB: 3 + 3 does not fit.
	if _, err := f.svc.Action(context.Background(), b.ID, models.VMAction{Action: "start"}, f.sub); code(err) != models.CodeInsufficientMemory {
		t.Errorf("start over the memory budget: %v", err)
	}
	if n := f.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'vm.start' AND outcome = 'denied' AND resource_id = ?`, b.ID); n != 1 {
		t.Errorf("%d denied start entries, want 1", n)
	}

	// bhyve missing: refused with the reason.
	f.fake.Set(func(fk *hostfake.Fake) { fk.Info.VMMLoaded = false })
	if _, err := f.svc.Action(context.Background(), b.ID, models.VMAction{Action: "start"}, f.sub); code(err) != models.CodeBhyveUnavailable {
		t.Errorf("start without vmm: %v", err)
	}
	f.fake.Set(func(fk *hostfake.Fake) { fk.Info.VMMLoaded = true })

	// The netgraph modules missing, with a NIC: refused (ADR 0011).
	if len(b.NICs) == 0 {
		t.Fatal("the test VM has no NIC")
	}
	f.fake.Set(func(fk *hostfake.Fake) { fk.NoNetgraph = true })
	if _, err := f.svc.Action(context.Background(), b.ID, models.VMAction{Action: "start"}, f.sub); code(err) != models.CodePortFilterUnavailable {
		t.Errorf("start without netgraph: %v", err)
	}
	f.fake.Set(func(fk *hostfake.Fake) { fk.NoNetgraph = false })

	// A dataset with its key unloaded: refused, audited, nothing started
	// (ADR 0010 decision 5).
	f.fake.Lock("devpool/skep/vm/"+b.ID, true)
	f.act(a.ID, models.VMActionForceStop, nil)
	f.observed(a.ID)
	if _, err := f.svc.Action(context.Background(), b.ID, models.VMAction{Action: "start"}, f.sub); code(err) != models.CodeStorageLocked {
		t.Errorf("start with a locked dataset: %v", err)
	}
	if n := f.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'vm.start' AND outcome = 'denied' AND resource_id = ?`, b.ID); n != 4 {
		t.Errorf("%d denied start entries, want 4 (memory, vmm, netgraph, key)", n)
	}
	f.fake.Lock("devpool/skep/vm/"+b.ID, false)
	if task, _ := f.act(b.ID, models.VMActionStart, nil); task.Status != models.TaskSucceeded {
		t.Errorf("start once the key is loaded: %+v", task)
	}
	f.act(b.ID, models.VMActionForceStop, nil)
	f.observed(b.ID)

	// A bridge that holds a management interface.
	f.act(a.ID, models.VMActionForceStop, nil)
	f.observed(a.ID)
	f.fake.Set(func(fk *hostfake.Fake) { fk.BridgeMembers["bridge0"] = []string{"vtnet0"} })
	task, _ := f.act(a.ID, models.VMActionStart, nil)
	if task.Status != models.TaskFailed || task.Error.Code != models.CodeBridgeNotAllowed {
		t.Errorf("start on a bridge with a management member: %+v", task)
	}
	f.fake.Set(func(fk *hostfake.Fake) { fk.BridgeMembers["bridge0"] = nil })

	// Bad actions and timeouts.
	ninety := 90
	for _, tc := range []models.VMAction{{Action: "reboot"}, {Action: "start", Timeout: &ninety}, {Action: "stop", Timeout: new(int)}} {
		if _, err := f.svc.Action(context.Background(), a.ID, tc, f.sub); code(err) != models.CodeInvalidRequest {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

func TestRestart(t *testing.T) {
	f := newFixture(t)
	vm := f.create(basicVM("r"))
	f.act(vm.ID, models.VMActionStart, nil)
	first := *f.observed(vm.ID).Observed.PID
	task, err := f.act(vm.ID, models.VMActionRestart, nil)
	if err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("restart: %v %+v", err, task)
	}
	got := f.observed(vm.ID)
	if got.Observed.State != models.VMRunning || *got.Observed.PID == first {
		t.Errorf("after restart: %+v", got.Observed)
	}
	// One task, both audit phases.
	if n := f.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'vm.restart' AND task_id = ?`, task.ID); n != 2 {
		t.Errorf("%d restart entries for the task, want 2", n)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
