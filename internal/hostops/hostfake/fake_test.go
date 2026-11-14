package hostfake

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

const vmA = "0b0b6f4e-5d0c-4a52-9a43-0d2f8a1f3c11"

func TestFakeBehavesLikeTheHost(t *testing.T) {
	dir := t.TempDir()
	conf := hostops.Confinement{
		Pools: []string{"devpool"}, DatasetRoot: "skep", Bridges: []string{"bridge0"},
		MediaDir: filepath.Join(dir, "media"), RunDir: filepath.Join(dir, "run"), StateDir: filepath.Join(dir, "state"),
	}
	f := New(conf, Options{PoolBytes: 10 << 30})
	ops := f.Ops()
	ctx := context.Background()

	code := func(err error) string {
		var me *models.Error
		if errors.As(err, &me) {
			return me.Code
		}
		return ""
	}
	if _, err := ops.ZFS.Pool(ctx, "zroot"); code(err) != models.CodePoolNotAllowed {
		t.Errorf("pool outside the allow-list: %v", err)
	}
	if _, err := ops.Net.Bridge(ctx, "bridge9"); code(err) != models.CodeBridgeNotAllowed {
		t.Errorf("bridge outside the allow-list: %v", err)
	}
	if _, err := ops.ZFS.CreateVMDataset(ctx, "devpool", vmA); err != nil {
		t.Fatal(err)
	}
	full := hostops.VolumeSpec{Pool: "devpool", VMID: vmA, DiskID: vmA, Slot: 0, SizeBytes: 8 << 30, BlockSize: 16384}
	if _, err := ops.ZFS.CreateVolume(ctx, full); err != nil {
		t.Fatal(err)
	}
	// 8 of 10 GiB are reserved: a second full disk does not fit, a thin one does.
	full.Slot = 1
	if _, err := ops.ZFS.CreateVolume(ctx, full); code(err) != models.CodeInsufficientSpace {
		t.Errorf("overcommitted full disk: %v", err)
	}
	full.Thin = true
	if _, err := ops.ZFS.CreateVolume(ctx, full); err != nil {
		t.Errorf("thin disk: %v", err)
	}
	if p, _ := ops.ZFS.Pool(ctx, "devpool"); p.AvailableBytes != 2<<30 {
		t.Errorf("available = %d", p.AvailableBytes)
	}
	list, _ := ops.ZFS.VMDatasets(ctx, "devpool", vmA)
	if len(list) != 3 {
		t.Errorf("datasets = %+v", list)
	}
	destroyed, err := ops.ZFS.DestroyVM(ctx, "devpool", vmA)
	if err != nil || len(destroyed) != 3 {
		t.Errorf("destroyed = %v, %v", destroyed, err)
	}
	if list, _ := ops.ZFS.VMDatasets(ctx, "devpool", vmA); len(list) != 0 {
		t.Errorf("datasets after destroy = %+v", list)
	}
	if !ops.VMM.Capabilities(ctx).Simulated {
		t.Error("the fake does not say it is simulated")
	}
	f.Set(func(f *Fake) { f.ZFSLoaded = false })
	if _, err := ops.ZFS.Pool(ctx, "devpool"); code(err) != models.CodeStorageUnavailable {
		t.Errorf("zfs not loaded: %v", err)
	}
	if err := ops.Files.PrepareVM(ctx, vmA); err != nil {
		t.Errorf("prepare: %v", err)
	}
}
