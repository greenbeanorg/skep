package hostfake

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

const (
	vmB   = "1c1c6f4e-5d0c-4a52-9a43-0d2f8a1f3c22"
	disk0 = "7e3c1a90-1111-4222-8333-444455556660"
	disk1 = "7e3c1a90-1111-4222-8333-444455556661"
	diskC = "7e3c1a90-1111-4222-8333-444455556662"
	s1    = "11111111-aaaa-4bbb-8ccc-000000000001"
	s2    = "11111111-aaaa-4bbb-8ccc-000000000002"
)

func TestFakeSnapshotsFollowTheRules(t *testing.T) {
	dir := t.TempDir()
	conf := hostops.Confinement{Pools: []string{"devpool"}, DatasetRoot: "skep",
		RunDir: filepath.Join(dir, "run"), StateDir: filepath.Join(dir, "state")}
	f := New(conf, Options{})
	z := f.Ops().ZFS
	ctx := context.Background()
	code := func(err error) string {
		var me *models.Error
		if errors.As(err, &me) {
			return me.Code
		}
		return ""
	}
	a := hostops.Owner{Kind: hostops.OwnerVM, ID: vmA}
	if _, err := z.CreateVMDataset(ctx, "devpool", vmA); err != nil {
		t.Fatal(err)
	}
	if _, err := z.CreateVolume(ctx, hostops.VolumeSpec{Pool: "devpool", VMID: vmA, DiskID: disk0, Slot: 0, SizeBytes: 1 << 30, BlockSize: 16384, Thin: true}); err != nil {
		t.Fatal(err)
	}
	if got, err := z.Snapshot(ctx, "devpool", a, s1); err != nil || len(got) != 2 {
		t.Fatalf("snapshot = %v, %v", got, err)
	}
	if _, err := z.Snapshot(ctx, "devpool", a, s2); err != nil {
		t.Fatal(err)
	}
	if _, err := z.Rollback(ctx, "devpool", a, s1, false); code(err) != models.CodeSnapshotNotLatest {
		t.Errorf("rollback to an older snapshot: %v", err)
	}
	// A disk added after s1 makes s1 incomplete.
	if _, err := z.CreateVolume(ctx, hostops.VolumeSpec{Pool: "devpool", VMID: vmA, DiskID: disk1, Slot: 1, SizeBytes: 1 << 30, BlockSize: 16384, Thin: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := z.Rollback(ctx, "devpool", a, s1, true); code(err) != models.CodeSnapshotIncomplete {
		t.Errorf("rollback past a disk add: %v", err)
	}
	if _, err := z.CloneVM(ctx, hostops.CloneSpec{Pool: "devpool", SourceVMID: vmA, SnapshotID: s2, TargetVMID: vmB, Disks: map[int]string{0: diskC}}); code(err) != models.CodeSnapshotIncomplete {
		t.Errorf("clone of an incomplete snapshot: %v", err)
	}
	// Take a full one and clone it.
	s3 := "11111111-aaaa-4bbb-8ccc-000000000003"
	if _, err := z.Snapshot(ctx, "devpool", a, s3); err != nil {
		t.Fatal(err)
	}
	clone, err := z.CloneVM(ctx, hostops.CloneSpec{Pool: "devpool", SourceVMID: vmA, SnapshotID: s3, TargetVMID: vmB,
		Disks: map[int]string{0: diskC, 1: "7e3c1a90-1111-4222-8333-444455556663"}})
	if err != nil || len(clone) != 3 || clone[1].DiskID != diskC || clone[1].VMID != vmB || clone[1].Origin == "" {
		t.Fatalf("clone = %+v, %v", clone, err)
	}
	if _, err := z.DestroySnapshot(ctx, "devpool", a, s3); code(err) != models.CodeSnapshotHasClones {
		t.Errorf("destroy the origin: %v", err)
	}
	if _, err := z.DestroyVM(ctx, "devpool", vmA); code(err) != models.CodeSnapshotHasClones {
		t.Errorf("destroy the source: %v", err)
	}
	// Once the clone is gone, the origin can go, and so can the source.
	if _, err := z.DestroyVM(ctx, "devpool", vmB); err != nil {
		t.Fatal(err)
	}
	if _, err := z.DestroySnapshot(ctx, "devpool", a, s3); err != nil {
		t.Errorf("destroy after the clone went: %v", err)
	}
	if _, err := z.DestroyVM(ctx, "devpool", vmA); err != nil {
		t.Fatal(err)
	}
	if inv, _ := z.Inventory(ctx, "devpool"); len(inv) != 0 {
		t.Errorf("left behind: %+v", inv)
	}

	// A locked root refuses creates and snapshots.
	f.Lock("devpool/skep", true)
	if _, err := z.CreateVMDataset(ctx, "devpool", vmA); code(err) != models.CodeStorageLocked {
		t.Errorf("create under a locked root: %v", err)
	}
}
