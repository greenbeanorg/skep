package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/greenbeanorg/skep/pkg/models"
)

// fakeVMs is the VM side of the API, in memory. Every write answers with
// task-1, which fakeBackend reports as succeeded.
type fakeVMs struct {
	mu    sync.Mutex
	vms   []models.VM
	acts  []string
	snaps []models.Snapshot
}

const fakeSnapID = "5a5a6f4e-5d0c-4a52-9a43-0d2f8a1f3c99"
const fakeCloneID = "6b6b6f4e-5d0c-4a52-9a43-0d2f8a1f3c88"

const fakeVMID = "0b0b6f4e-5d0c-4a52-9a43-0d2f8a1f3c11"

func (f *fakeVMs) task(typ, id string) models.Task {
	return models.Task{ID: "task-1", Type: typ, Status: models.TaskPending, ResourceType: models.VMResource, ResourceID: id}
}

func (f *fakeVMs) VMs(context.Context, models.VMListOptions) (models.VMList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return models.VMList{Items: append([]models.VM{}, f.vms...)}, nil
}

func (f *fakeVMs) VM(_ context.Context, id string) (models.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, vm := range f.vms {
		if vm.ID == id {
			return vm, nil
		}
	}
	return models.VM{}, models.ErrNotFound
}

func (f *fakeVMs) CreateVM(_ context.Context, req models.VMCreate) (models.VMCreated, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm := models.VM{ID: fakeVMID, Name: req.Name, Engine: "bhyve", Firmware: "uefi", CPUCount: req.CPUCount,
		MemoryBytes: int64(req.Memory), DesiredState: "stopped", Observed: models.VMObserved{State: "stopped"},
		Disks: []models.VMDisk{{Slot: 0, Bus: "virtio-blk", SizeBytes: 8 << 30, Thin: true, Dataset: "p/skep/vm/" + fakeVMID + "/disk0"}},
		NICs:  []models.VMNIC{}}
	f.vms = append(f.vms, vm)
	return models.VMCreated{VM: vm, Task: f.task(models.TaskVMCreate, vm.ID)}, nil
}

func (f *fakeVMs) UpdateVM(context.Context, string, models.VMUpdate) (models.Task, error) {
	return models.Task{}, models.ErrNotFound
}

func (f *fakeVMs) DeleteVM(_ context.Context, id string) (models.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acts = append(f.acts, "delete "+id)
	return f.task(models.TaskVMDelete, id), nil
}

func (f *fakeVMs) VMAction(_ context.Context, id string, act models.VMAction) (models.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := act.Action
	if act.Timeout != nil {
		a += " timeout"
	}
	f.acts = append(f.acts, a+" "+id)
	return f.task("vm."+act.Action, id), nil
}

func (f *fakeVMs) StoragePools(context.Context) (models.StoragePoolList, error) {
	return models.StoragePoolList{}, nil
}

func (f *fakeVMs) StorageDatasets(context.Context, models.DatasetListOptions) (models.DatasetList, error) {
	return models.DatasetList{}, nil
}

func (f *fakeVMs) Snapshots(context.Context, string) (models.SnapshotList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return models.SnapshotList{Items: append([]models.Snapshot{}, f.snaps...)}, nil
}

func (f *fakeVMs) Snapshot(context.Context, string, string) (models.Snapshot, error) {
	return models.Snapshot{}, models.ErrNotFound
}

func (f *fakeVMs) CreateSnapshot(_ context.Context, vmID string, req models.SnapshotCreate) (models.SnapshotCreated, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sn := models.Snapshot{ID: fakeSnapID, ResourceKind: "vm", ResourceID: vmID, Name: req.Name, Description: req.Description,
		Consistency: "crash_consistent", Provisioned: true, Clones: []models.SnapshotClone{}}
	f.snaps = append(f.snaps, sn)
	f.acts = append(f.acts, "snapshot "+req.Name)
	return models.SnapshotCreated{Snapshot: sn, Task: f.task(models.TaskVMSnapshotCreate, vmID)}, nil
}

func (f *fakeVMs) RollbackSnapshot(_ context.Context, vmID, id string, req models.SnapshotRollback) (models.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := "rollback " + id
	if req.DestroyNewer {
		a += " destroy_newer"
	}
	f.acts = append(f.acts, a)
	return f.task(models.TaskVMSnapshotRollback, vmID), nil
}

func (f *fakeVMs) DeleteSnapshot(_ context.Context, vmID, id string) (models.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acts = append(f.acts, "delete-snapshot "+id)
	return f.task(models.TaskVMSnapshotDelete, vmID), nil
}

func (f *fakeVMs) CloneVM(_ context.Context, vmID string, req models.VMClone) (models.VMCreated, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acts = append(f.acts, "clone "+req.Name+" from "+req.SnapshotID)
	vm := models.VM{ID: fakeCloneID, Name: req.Name, Observed: models.VMObserved{State: "stopped"}}
	return models.VMCreated{VM: vm, Task: f.task(models.TaskVMClone, vmID)}, nil
}

func (f *fakeVMs) Metrics(context.Context) (models.Metrics, error) { return models.Metrics{}, nil }

func (f *fakeVMs) Media(context.Context) (models.MediaList, error) { return models.MediaList{}, nil }

func TestVMCommands(t *testing.T) {
	s := newStack(t)
	stateDir := filepath.Join(t.TempDir(), "skepctl")
	login(t, s, stateDir, "andy")

	def := `{"name":"web1","cpu_count":2,"memory":"1G","disks":[{"size":"8G","pool":"skeptest","thin":true}]}`
	out, errOut, err := skepctl(stateDir, nil, def, "vm", "create", "-f", "-", "--wait")
	if err != nil || !strings.Contains(out, "VM web1 ("+fakeVMID+") created; task task-1 is succeeded") {
		t.Fatalf("create: %v\n%s%s", err, out, errOut)
	}
	// A typo is an error, not an ignored setting.
	if _, _, err := skepctl(stateDir, nil, `{"name":"x","cpus":2}`, "vm", "create", "-f", "-"); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("unknown field: %v", err)
	}

	out, _, err = skepctl(stateDir, nil, "", "vm", "list")
	if err != nil || !strings.Contains(out, "web1") || !strings.Contains(out, fakeVMID) {
		t.Fatalf("list: %v\n%s", err, out)
	}
	out, _, err = skepctl(stateDir, nil, "", "vm", "show", "web1")
	if err != nil || !strings.Contains(out, "Observed state:  stopped") || !strings.Contains(out, "p/skep/vm/"+fakeVMID+"/disk0") {
		t.Fatalf("show by name: %v\n%s", err, out)
	}
	if _, _, err := skepctl(stateDir, nil, "", "vm", "show", "nope"); err == nil {
		t.Error("an unknown name was found")
	}

	for _, args := range [][]string{
		{"vm", "start", "web1", "--wait"},
		{"vm", "stop", fakeVMID, "--timeout", "30"},
		{"vm", "force-stop", "web1"},
		{"vm", "restart", "web1"},
		{"vm", "delete", "web1", "--json"},
	} {
		if out, errOut, err := skepctl(stateDir, nil, "", args...); err != nil {
			t.Errorf("%v: %v\n%s%s", args, err, out, errOut)
		}
	}
	want := []string{"start " + fakeVMID, "stop timeout " + fakeVMID, "force_stop " + fakeVMID, "restart " + fakeVMID, "delete " + fakeVMID}
	if got := strings.Join(s.vms(t).acts, ","); got != strings.Join(want, ",") {
		t.Errorf("actions = %s", got)
	}

	// A viewer is refused by the server.
	viewerDir := filepath.Join(t.TempDir(), "viewer")
	login(t, s, viewerDir, "vera")
	if _, _, err := skepctl(viewerDir, nil, "", "vm", "start", fakeVMID); err == nil {
		t.Error("a viewer started a VM")
	}
	if out, _, err := skepctl(viewerDir, nil, "", "vm", "list"); err != nil || !strings.Contains(out, "web1") {
		t.Errorf("a viewer cannot list VMs: %v", err)
	}
}

func TestSnapshotCommands(t *testing.T) {
	s := newStack(t)
	stateDir := filepath.Join(t.TempDir(), "skepctl")
	login(t, s, stateDir, "andy")
	def := `{"name":"web1","cpu_count":1,"memory":"1G","disks":[{"size":"8G","pool":"skeptest"}]}`
	if _, _, err := skepctl(stateDir, nil, def, "vm", "create", "-f", "-"); err != nil {
		t.Fatal(err)
	}

	out, errOut, err := skepctl(stateDir, nil, "", "vm", "snapshot", "create", "web1", "before-upgrade", "--description", "x", "--wait")
	if err != nil || !strings.Contains(out, "snapshot before-upgrade ("+fakeSnapID+") of web1, crash-consistent (taken while running); task task-1 is succeeded") {
		t.Fatalf("create: %v\n%s%s", err, out, errOut)
	}
	out, _, err = skepctl(stateDir, nil, "", "vm", "snapshot", "list", "web1")
	if err != nil || !strings.Contains(out, "before-upgrade") || !strings.Contains(out, fakeSnapID) || !strings.Contains(out, "CONSISTENCY") {
		t.Fatalf("list: %v\n%s", err, out)
	}
	for _, args := range [][]string{
		{"vm", "snapshot", "rollback", "web1", "before-upgrade"},
		{"vm", "snapshot", "rollback", fakeVMID, fakeSnapID, "--destroy-newer", "--wait"},
		{"vm", "snapshot", "delete", "web1", "before-upgrade", "--json"},
	} {
		if out, errOut, err := skepctl(stateDir, nil, "", args...); err != nil {
			t.Errorf("%v: %v\n%s%s", args, err, out, errOut)
		}
	}
	if _, _, err := skepctl(stateDir, nil, "", "vm", "snapshot", "delete", "web1", "nope"); err == nil || !strings.Contains(err.Error(), `no snapshot "nope"`) {
		t.Errorf("unknown snapshot: %v", err)
	}
	if _, _, err := skepctl(stateDir, nil, "", "vm", "snapshot", "create", "web1"); err == nil {
		t.Error("create without a name was accepted")
	}

	out, errOut, err = skepctl(stateDir, nil, "", "vm", "clone", "web1", "web2", "--snapshot", "before-upgrade")
	if err != nil || !strings.Contains(out, "VM web2 ("+fakeCloneID+") cloned from web1") || !strings.Contains(out, "same guest") || !strings.Contains(out, "SSH host keys") {
		t.Fatalf("clone: %v\n%s%s", err, out, errOut)
	}
	out, errOut, err = skepctl(stateDir, nil, "", "vm", "clone", "web1", "web3", "--json")
	if err != nil || strings.Contains(out, "same guest") || !strings.Contains(errOut, "same guest") || !strings.Contains(out, `"name": "web3"`) {
		t.Fatalf("clone --json: %v\n%s%s", err, out, errOut)
	}

	want := []string{"snapshot before-upgrade", "rollback " + fakeSnapID, "rollback " + fakeSnapID + " destroy_newer",
		"delete-snapshot " + fakeSnapID, "clone web2 from " + fakeSnapID, "clone web3 from "}
	if got := strings.Join(s.vms(t).acts, ","); got != strings.Join(want, ",") {
		t.Errorf("actions = %s\nwant      %s", got, strings.Join(want, ","))
	}

	// A viewer lists, and the server refuses its snapshot.
	viewerDir := filepath.Join(t.TempDir(), "viewer")
	login(t, s, viewerDir, "vera")
	if out, _, err := skepctl(viewerDir, nil, "", "vm", "snapshot", "list", "web1"); err != nil || !strings.Contains(out, "before-upgrade") {
		t.Errorf("a viewer cannot list snapshots: %v", err)
	}
	if _, _, err := skepctl(viewerDir, nil, "", "vm", "snapshot", "create", "web1", "v"); err == nil {
		t.Error("a viewer took a snapshot")
	}
}

// A failed task's details are shown under its error, one per line, sorted
// and cleaned of terminal control characters, by "tasks get" and by the
// commands that wait for a task.
func TestTaskErrorDetailsShown(t *testing.T) {
	task := models.Task{ID: "t1", Status: models.TaskFailed, Error: &models.TaskError{
		Code: "disk_device_missing", Message: "no device node",
		Details: map[string]any{"reason": "volmode_none", "command": "zfs set volmode=dev tank/a", "dataset": "tank/a\x1b[2J"}}}
	err := taskFailure(task)
	want := "task t1 failed: disk_device_missing: no device node\n  command: zfs set volmode=dev tank/a\n  dataset: tank/a [2J\n  reason: volmode_none"
	if err == nil || err.Error() != want {
		t.Errorf("taskFailure:\n%v\nwant:\n%s", err, want)
	}
	var b strings.Builder
	if err := printTask(&b, models.TaskDetail{Task: task}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"Error:", "disk_device_missing: no device node", "  command:", "zfs set volmode=dev tank/a", "  reason:", "volmode_none"} {
		if !strings.Contains(b.String(), s) {
			t.Errorf("tasks get lacks %q:\n%s", s, b.String())
		}
	}
	if strings.Contains(b.String(), "\x1b") {
		t.Error("an escape sequence reached the terminal")
	}
	task.Error.Details = nil
	if err := taskFailure(task); err == nil || strings.Contains(err.Error(), "\n") {
		t.Errorf("without details: %v", err)
	}
}
