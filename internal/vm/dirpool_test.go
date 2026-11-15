package vm

import (
	"context"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/hostops/hostfake"
	"github.com/greenbeanorg/skep/pkg/models"
)

func dirVM(name string) models.VMCreate {
	req := basicVM(name)
	req.Disks[0] = models.VMDiskSpec{Pool: "dirpool", Size: models.ByteSize(8 * gib), Thin: true}
	return req
}

// TestDirectoryPoolVM covers a VM whose disks are files in a directory
// pool (ADR 0010 decision 6): it is created, started, changed, and
// deleted like any other; snapshots and clones are refused.
func TestDirectoryPoolVM(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	pools, err := f.svc.StoragePools(ctx)
	if err != nil || len(pools.Items) != 2 {
		t.Fatalf("pools = %+v, %v", pools, err)
	}
	zp, dp := pools.Items[0], pools.Items[1]
	if zp.Type != "zfs" || !zp.Capabilities.Snapshots || dp.Type != "directory" || dp.Name != "dirpool" ||
		dp.Capabilities != (models.StoragePoolCapabilities{}) || dp.Health != "ONLINE" {
		t.Errorf("pools = %+v", pools.Items)
	}
	if n := f.count(`SELECT COUNT(*) FROM storage_pools WHERE name = 'dirpool' AND type = 'directory'`); n != 1 {
		t.Errorf("directory pool rows = %d", n)
	}

	// volblocksize is a ZFS option.
	bad := dirVM("bad")
	bad.Disks[0].VolBlockSize = models.ByteSize(16 << 10)
	if _, err := f.svc.CreateVM(ctx, bad, f.sub); code(err) != models.CodeInvalidRequest {
		t.Errorf("volblocksize on a directory pool = %v", err)
	}

	vm := f.create(dirVM("files1"))
	d := vm.Disks[0]
	if d.PoolType != "directory" || d.File != "skep/vm/"+vm.ID+"/disk0.img" || d.Dataset != "" || d.VolBlockSize != 0 {
		t.Fatalf("disk = %+v", d)
	}
	if n := f.count(`SELECT COUNT(*) FROM storage_volumes WHERE kind = 'file' AND name = ? AND desired_volblocksize IS NULL`,
		"skep/vm/"+vm.ID+"/disk0.img"); n != 1 {
		t.Errorf("file volume rows = %d", n)
	}
	files := f.fake.Files()
	if len(files) != 1 || files[0] != "/srv/dirpool/skep/vm/"+vm.ID+"/disk0.img" {
		t.Errorf("files = %v", files)
	}
	if path, err := f.conf.DiskPath("dirpool", vm.ID, 0); err != nil || path != files[0] {
		t.Errorf("disk path = %q, %v", path, err)
	}

	// It starts and stops.
	if task, err := f.act(vm.ID, models.VMActionStart, nil); err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("start: %+v, %v", task, err)
	}
	if _, err := f.snapshotErr(vm.ID, "s1"); code(err) != models.CodeStorageFeatureUnsupported ||
		!strings.Contains(err.Error(), "directory pool") {
		t.Errorf("snapshot of a running directory-pool VM = %v", err)
	}
	if task, err := f.act(vm.ID, models.VMActionForceStop, nil); err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("force_stop: %+v, %v", task, err)
	}

	// Snapshots and clones are refused, never emulated.
	if _, err := f.snapshotErr(vm.ID, "s1"); code(err) != models.CodeStorageFeatureUnsupported {
		t.Errorf("snapshot = %v", err)
	}
	if _, err := f.svc.CloneVM(ctx, vm.ID, models.VMClone{Name: "copy"}, f.sub); code(err) != models.CodeStorageFeatureUnsupported {
		t.Errorf("clone = %v", err)
	}

	// Disks are added, grown, and removed as in a ZFS pool.
	task, err := f.update(vm.ID, models.VMUpdate{AddDisks: []models.VMDiskSpec{{Pool: "dirpool", Size: models.ByteSize(gib)}}})
	if err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("add: %v %+v", err, task)
	}
	got, _ := f.svc.VM(ctx, vm.ID)
	if len(got.Disks) != 3 || got.Disks[2].PoolType != "directory" || got.Disks[2].File != "skep/vm/"+vm.ID+"/disk2.img" {
		t.Fatalf("disks after add = %+v", got.Disks)
	}
	if p, _ := f.fake.Ops().Disks().Pool(ctx, "dirpool"); p.AvailableBytes != poolSize-gib {
		t.Errorf("a full-size file did not take its space: %+v", p)
	}
	task, err = f.update(vm.ID, models.VMUpdate{ResizeDisks: []models.VMDiskResize{{Slot: 0, Size: models.ByteSize(10 * gib)}}})
	if err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("grow: %v %+v", err, task)
	}
	if _, err := f.update(vm.ID, models.VMUpdate{AddDisks: []models.VMDiskSpec{{Pool: "dirpool", Size: models.ByteSize(30 * gib)}}}); code(err) != models.CodeInsufficientSpace {
		t.Errorf("a disk larger than the pool = %v", err)
	}
	task, err = f.update(vm.ID, models.VMUpdate{RemoveDisks: []int{2}})
	if err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("remove: %v %+v", err, task)
	}
	got, _ = f.svc.VM(ctx, vm.ID)
	if len(got.Disks) != 2 || got.Disks[0].SizeBytes != 10*gib {
		t.Errorf("disks after grow and remove = %+v", got.Disks)
	}
	if files := f.fake.Files(); len(files) != 1 {
		t.Errorf("files after remove = %v", files)
	}

	// A VM with disks in both kinds of pool has no snapshots either: they
	// would not be atomic across pools.
	mixed := basicVM("mixed")
	mixed.Disks = append(mixed.Disks, models.VMDiskSpec{Pool: "dirpool", Size: models.ByteSize(gib), Thin: true})
	mvm := f.create(mixed)
	if _, err := f.snapshotErr(mvm.ID, "s1"); code(err) != models.CodeStorageFeatureUnsupported {
		t.Errorf("snapshot of a mixed VM = %v", err)
	}

	// Delete removes the files and the directory.
	dtask, err := f.svc.DeleteVM(ctx, vm.ID, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	if done := f.wait(dtask.ID); done.Status != models.TaskSucceeded {
		t.Fatalf("delete: %+v", done)
	}
	for _, file := range f.fake.Files() {
		if strings.Contains(file, vm.ID) {
			t.Errorf("a file of the deleted VM is left: %s", file)
		}
	}
	calls := strings.Join(f.fake.Calls(), "\n")
	if !strings.Contains(calls, "rm -r /srv/dirpool/skep/vm/"+vm.ID) {
		t.Errorf("calls = %s", calls)
	}
}

// TestDirectoryPoolUnusable checks that a directory pool that cannot be
// used refuses new disks and says why in the pool list.
func TestDirectoryPoolUnusable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.fake.Set(func(fk *hostfake.Fake) { fk.UnusableDirs["dirpool"] = "/srv/dirpool is not owned by root" })
	if _, err := f.svc.CreateVM(ctx, dirVM("x"), f.sub); code(err) != models.CodeStorageUnavailable {
		t.Errorf("create on an unusable pool = %v", err)
	}
	pools, _ := f.svc.StoragePools(ctx)
	if p := pools.Items[1]; p.Health != "UNAVAILABLE" || !strings.Contains(p.Error, "owned by root") {
		t.Errorf("pool = %+v", p)
	}
	_ = hostops.PoolKindDirectory
}

func (f *fixture) snapshotErr(vmID, name string) (models.SnapshotCreated, error) {
	return f.svc.CreateSnapshot(context.Background(), vmID, models.SnapshotCreate{Name: name}, f.sub)
}
