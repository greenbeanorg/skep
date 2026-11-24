package vm

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/greenbeanorg/skep/pkg/models"
)

// progress records what a task reported.
type progress struct {
	mu   sync.Mutex
	msgs []string
}

func (p *progress) Progress(message string, data any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, message)
}

func (f *fixture) diskDataset(vm models.VM) string {
	f.t.Helper()
	for _, d := range vm.Disks {
		if d.Bus != models.BusAHCICD {
			ds, err := f.conf.DiskDataset(d.Pool, vm.ID, d.Slot)
			if err != nil {
				f.t.Fatal(err)
			}
			return ds
		}
	}
	f.t.Fatal("the VM has no disk")
	return ""
}

func (f *fixture) called(call string) bool {
	for _, c := range f.fake.Calls() {
		if c == call {
			return true
		}
	}
	return false
}

func (f *fixture) callIndex(prefix string) int {
	for i, c := range f.fake.Calls() {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

// TestDeviceNodeIsEnsured covers ADR 0010 amendment z in the VM service:
// the node is checked after create, a disk added, a clone, and a
// rollback without failing them, and before every start, which fails
// when the node cannot be brought back.
func TestDeviceNodeIsEnsured(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	vm := f.create(basicVM("web1"))
	ds := f.diskDataset(vm)
	if !f.called("ensure device " + ds) {
		t.Fatalf("vm.create did not check the device node: %v", f.fake.Calls())
	}

	// A missing node is repaired before the runner starts.
	f.fake.MissingDevices = map[string]error{ds: nil}
	task, err := f.act(vm.ID, models.VMActionStart, nil)
	if err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("start: %v %+v", err, task)
	}
	if i, b := f.callIndex("zfs set volmode=dev "+ds), f.callIndex("bhyve -k"); i < 0 || b < 0 || i > b {
		t.Errorf("the repair did not come before the runner: %v", f.fake.Calls())
	}

	// While the VM runs, a repair is refused and nothing is changed.
	f.fake.MissingDevices = map[string]error{ds: nil}
	rep := &progress{}
	f.svc.ensureDevices(ctx, vm.ID, vm.Disks, rep)
	if _, still := f.fake.MissingDevices[ds]; !still {
		t.Error("a running VM's disk was repaired")
	}
	if len(rep.msgs) != 1 || !strings.Contains(rep.msgs[0], "missing") {
		t.Errorf("progress = %v", rep.msgs)
	}
	if task, err := f.act(vm.ID, models.VMActionForceStop, nil); err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("force-stop: %v %+v", err, task)
	}

	// A node that cannot be brought back fails the start, before any
	// runner.
	f.fake.MissingDevices = map[string]error{ds: &models.Error{Code: models.CodeDiskDeviceMissing, Message: "volmode=none",
		Details: map[string]any{"dataset": ds, "reason": "volmode_none"}}}
	task, err = f.act(vm.ID, models.VMActionStart, nil)
	if err != nil || task.Status != models.TaskFailed || task.Error == nil || task.Error.Code != models.CodeDiskDeviceMissing {
		t.Fatalf("start with a missing node: %v %+v", err, task)
	}
	if got := f.observed(vm.ID); got.Observed.State != models.VMStopped {
		t.Errorf("state = %s", got.Observed.State)
	}

	// Snapshot, rollback, and clone still succeed with the node missing;
	// each checks it.
	s1, _ := f.snapshot(vm.ID, "golden")
	before := len(f.fake.Calls())
	rb, err := f.svc.RollbackSnapshot(ctx, vm.ID, s1.ID, models.SnapshotRollback{}, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	if done := f.wait(rb.ID); done.Status != models.TaskSucceeded {
		t.Fatalf("rollback: %s %s", done.ErrorCode, done.ErrorMessage)
	}
	if !containsFrom(f.fake.Calls(), before, "ensure device "+ds) {
		t.Error("the rollback did not check the device node")
	}
	created, err := f.svc.CloneVM(ctx, vm.ID, models.VMClone{Name: "web1-clone", SnapshotID: s1.ID}, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	if done := f.wait(created.Task.ID); done.Status != models.TaskSucceeded {
		t.Fatalf("clone: %s %s", done.ErrorCode, done.ErrorMessage)
	}
	clone, _ := f.svc.VM(ctx, created.VM.ID)
	if !f.called("ensure device " + f.diskDataset(clone)) {
		t.Error("the clone did not check its device node")
	}

	// A disk added to the stopped VM is checked.
	upd, err := f.svc.UpdateVM(ctx, clone.ID, models.VMUpdate{AddDisks: []models.VMDiskSpec{{Pool: "devpool", Size: models.ByteSize(gib), Thin: true}}}, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	if done := f.wait(upd.ID); done.Status != models.TaskSucceeded {
		t.Fatalf("update: %s %s", done.ErrorCode, done.ErrorMessage)
	}
	clone, _ = f.svc.VM(ctx, clone.ID)
	for _, d := range clone.Disks {
		if d.Slot == 0 || d.Bus == models.BusAHCICD {
			continue
		}
		added, _ := f.conf.DiskDataset(d.Pool, clone.ID, d.Slot)
		if !f.called("ensure device " + added) {
			t.Errorf("the added disk %s was not checked", added)
		}
	}
}

func containsFrom(calls []string, from int, call string) bool {
	for _, c := range calls[from:] {
		if c == call {
			return true
		}
	}
	return false
}
