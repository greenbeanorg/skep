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
	mu   sync.Mutex
	vms  []models.VM
	acts []string
}

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
