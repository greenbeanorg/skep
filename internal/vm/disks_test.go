package vm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
)

func (f *fixture) update(vmID string, upd models.VMUpdate) (tasks.Task, error) {
	f.t.Helper()
	task, err := f.svc.UpdateVM(context.Background(), vmID, upd, f.sub)
	if err != nil {
		return tasks.Task{}, err
	}
	return f.wait(task.ID), nil
}

func TestDiskAddGrowRemove(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	vm := f.create(basicVM("web1")) // disk0 (8G thin), cd in slot 1

	// Add: the lowest free slot, which is 2 (slot 1 holds the media).
	task, err := f.update(vm.ID, models.VMUpdate{AddDisks: []models.VMDiskSpec{{Pool: "devpool", Size: models.ByteSize(gib)}}})
	if err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("add: %v %+v", err, task)
	}
	got, _ := f.svc.VM(ctx, vm.ID)
	if len(got.Disks) != 3 || got.Disks[2].Slot != 2 || got.Disks[2].SizeBytes != gib || got.Disks[2].Thin ||
		got.Disks[2].Dataset != "devpool/skep/vm/"+vm.ID+"/disk2" {
		t.Fatalf("disks after add = %+v", got.Disks)
	}
	var res UpdateResult
	_ = json.Unmarshal(task.Result, &res)
	if len(res.DisksAdded) != 1 || res.DisksAdded[0].SizeBytes != gib {
		t.Errorf("result = %+v", res)
	}

	// Grow; never shrink; not while running.
	s1, _ := f.snapshot(vm.ID, "before-grow")
	task, err = f.update(vm.ID, models.VMUpdate{ResizeDisks: []models.VMDiskResize{{Slot: 0, Size: models.ByteSize(12 * gib)}}})
	if err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("grow: %v %+v", err, task)
	}
	_ = json.Unmarshal(task.Result, &res)
	if len(res.Notes) == 0 || !strings.Contains(res.Notes[0], "growfs") {
		t.Errorf("no note about the guest's filesystem: %+v", res)
	}
	got, _ = f.svc.VM(ctx, vm.ID)
	if got.Disks[0].SizeBytes != 12*gib {
		t.Errorf("size after grow = %d", got.Disks[0].SizeBytes)
	}
	for name, upd := range map[string]models.VMUpdate{
		"shrink":        {ResizeDisks: []models.VMDiskResize{{Slot: 0, Size: models.ByteSize(4 * gib)}}},
		"same":          {ResizeDisks: []models.VMDiskResize{{Slot: 0, Size: models.ByteSize(12 * gib)}}},
		"media":         {ResizeDisks: []models.VMDiskResize{{Slot: 1, Size: models.ByteSize(12 * gib)}}},
		"no such slot":  {RemoveDisks: []int{9}},
		"twice":         {RemoveDisks: []int{2, 2}},
		"grow+remove":   {RemoveDisks: []int{2}, ResizeDisks: []models.VMDiskResize{{Slot: 2, Size: models.ByteSize(2 * gib)}}},
		"media via add": {AddDisks: []models.VMDiskSpec{{Media: testISO}}},
		"too big full":  {AddDisks: []models.VMDiskSpec{{Pool: "devpool", Size: models.ByteSize(poolSize)}}},
		"other pool":    {AddDisks: []models.VMDiskSpec{{Pool: "zroot", Size: models.ByteSize(gib)}}},
	} {
		if _, err := f.svc.UpdateVM(ctx, vm.ID, upd, f.sub); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// A rollback to before the grow gives the disk its old size back.
	rb, err := f.svc.RollbackSnapshot(ctx, vm.ID, s1.ID, models.SnapshotRollback{}, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	if done := f.wait(rb.ID); done.Status != models.TaskSucceeded {
		t.Fatalf("rollback: %+v", done.ErrorCode+" "+done.ErrorMessage)
	}
	got, _ = f.svc.VM(ctx, vm.ID)
	if got.Disks[0].SizeBytes != 8*gib {
		t.Errorf("size after rollback = %d, want the snapshot's", got.Disks[0].SizeBytes)
	}

	// Remove slot 2: s1 contains it, so destroy_snapshots is needed.
	_, err = f.svc.UpdateVM(ctx, vm.ID, models.VMUpdate{RemoveDisks: []int{2}}, f.sub)
	if code(err) != models.CodeDiskHasSnapshots {
		t.Fatalf("remove without destroy_snapshots: %v", err)
	}
	// A snapshot taken before the disk was added is kept; one after goes.
	task, err = f.update(vm.ID, models.VMUpdate{RemoveDisks: []int{2}, DestroySnapshots: true})
	if err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("remove: %v %+v", err, task)
	}
	_ = json.Unmarshal(task.Result, &res)
	if len(res.SnapshotsDestroyed) != 1 || res.SnapshotsDestroyed[0].ID != s1.ID {
		t.Errorf("snapshots destroyed = %+v", res.SnapshotsDestroyed)
	}
	got, _ = f.svc.VM(ctx, vm.ID)
	if len(got.Disks) != 2 {
		t.Errorf("disks after remove = %+v", got.Disks)
	}
	for _, name := range f.fake.Datasets() {
		if strings.Contains(name, "disk2") || strings.Contains(name, "@") {
			t.Errorf("left behind: %s", name)
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM snapshots WHERE resource_id = ?`, vm.ID); n != 0 {
		t.Errorf("%d snapshot rows left", n)
	}
	var details string
	if err := f.db.QueryRow(`SELECT details FROM audit_log WHERE action = 'vm.update' AND phase = 'finished' AND task_id = ?`,
		task.ID).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, "datasets_destroyed") || !strings.Contains(details, "/disk2") || !strings.Contains(details, s1.ID) {
		t.Errorf("audit details = %s", details)
	}
}

func TestRemoveKeepsOlderSnapshots(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	vm := f.create(basicVM("web1"))
	old, _ := f.snapshot(vm.ID, "old")
	if task, err := f.update(vm.ID, models.VMUpdate{AddDisks: []models.VMDiskSpec{{Pool: "devpool", Size: models.ByteSize(gib), Thin: true}}}); err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("add: %v %+v", err, task)
	}
	newer, _ := f.snapshot(vm.ID, "newer")
	task, err := f.update(vm.ID, models.VMUpdate{RemoveDisks: []int{2}, DestroySnapshots: true})
	if err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("remove: %v %+v", err, task)
	}
	list, _ := f.svc.Snapshots(ctx, vm.ID)
	if len(list.Items) != 1 || list.Items[0].ID != old.ID || !list.Items[0].Observed.Exists {
		t.Errorf("snapshots = %+v (newer %s should be gone)", list.Items, newer.ID)
	}
	// The kept snapshot is whole: every dataset of the VM still has it,
	// and the VM can be rolled back to it.
	rb, err := f.svc.RollbackSnapshot(ctx, vm.ID, old.ID, models.SnapshotRollback{}, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	if done := f.wait(rb.ID); done.Status != models.TaskSucceeded {
		t.Errorf("rollback to the kept snapshot: %+v", done.ErrorCode+" "+done.ErrorMessage)
	}
	// A snapshot with a clone blocks a removal whatever the flag says.
	if task, err := f.update(vm.ID, models.VMUpdate{AddDisks: []models.VMDiskSpec{{Pool: "devpool", Size: models.ByteSize(gib), Thin: true}}}); err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("add: %v %+v", err, task)
	}
	c, err := f.svc.CloneVM(ctx, vm.ID, models.VMClone{Name: "c1"}, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	f.wait(c.Task.ID)
	if _, err := f.svc.UpdateVM(ctx, vm.ID, models.VMUpdate{RemoveDisks: []int{2}, DestroySnapshots: true}, f.sub); code(err) != models.CodeSnapshotHasClones {
		t.Errorf("remove under a clone: %v", err)
	}
}
