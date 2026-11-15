package hostfake

import (
	"context"
	"errors"
	"testing"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

const diskA = "1c1c6f4e-5d0c-4a52-9a43-0d2f8a1f3c22"

// TestDisksRouter checks that hostops.Disks sends each operation to the
// kind of its pool.
func TestDisksRouter(t *testing.T) {
	conf := hostops.Confinement{
		Pools: []string{"devpool"}, DatasetRoot: "skep",
		Directories: []hostops.DirPool{{Name: "dirpool", Path: "/srv/dirpool"}},
		RunDir:      t.TempDir(), StateDir: t.TempDir(), MediaDir: t.TempDir(),
	}
	f := New(conf, Options{PoolBytes: 10 << 30})
	disks := f.Ops().Disks()
	ctx := context.Background()

	pools, err := disks.Pools(ctx)
	if err != nil || len(pools) != 2 || pools[0].Kind != hostops.PoolKindZFS || pools[1].Kind != hostops.PoolKindDirectory ||
		pools[1].Name != "dirpool" || pools[1].AvailableBytes != 10<<30 {
		t.Fatalf("pools = %+v, %v", pools, err)
	}
	if p, err := disks.Pool(ctx, "devpool"); err != nil || p.Kind != hostops.PoolKindZFS {
		t.Errorf("devpool = %+v, %v", p, err)
	}
	if _, err := disks.Pool(ctx, "tank"); !hasCode(err, models.CodePoolNotAllowed) {
		t.Errorf("tank = %v", err)
	}

	for _, pool := range []string{"devpool", "dirpool"} {
		if _, err := disks.PrepareVM(ctx, pool, vmA); err != nil {
			t.Fatal(err)
		}
		made, err := disks.Create(ctx, hostops.VolumeSpec{Pool: pool, VMID: vmA, DiskID: diskA, Slot: 0,
			SizeBytes: 1 << 30, BlockSize: 16 << 10})
		if err != nil {
			t.Fatalf("%s: %v", pool, err)
		}
		wantType := map[string]string{"devpool": "volume", "dirpool": hostops.TypeFile}[pool]
		if made.Type != wantType {
			t.Errorf("%s: created %+v", pool, made)
		}
		grown, err := disks.Resize(ctx, hostops.VolumeSpec{Pool: pool, VMID: vmA, DiskID: diskA, Slot: 0, SizeBytes: 2 << 30})
		if err != nil || grown.VolSizeBytes != 2<<30 {
			t.Errorf("%s: grow = %+v, %v", pool, grown, err)
		}
		vols, err := disks.VMVolumes(ctx, pool, vmA)
		if err != nil || len(vols) == 0 {
			t.Errorf("%s: volumes = %+v, %v", pool, vols, err)
		}
	}
	if p, _ := disks.Pool(ctx, "dirpool"); p.AvailableBytes != 8<<30 {
		t.Errorf("a full-size file did not take its space: %+v", p)
	}
	if got := f.Files(); len(got) != 1 || got[0] != "/srv/dirpool/skep/vm/"+vmA+"/disk0.img" {
		t.Errorf("files = %v", got)
	}
	if _, err := disks.Destroy(ctx, hostops.VolumeSpec{Pool: "dirpool", VMID: vmA, DiskID: diskA, Slot: 0}); err != nil {
		t.Fatal(err)
	}
	gone, err := disks.DestroyVM(ctx, "dirpool", vmA)
	if err != nil || len(gone) != 1 || gone[0].Type != "directory" {
		t.Errorf("DestroyVM = %+v, %v", gone, err)
	}

	f.Set(func(f *Fake) { f.UnusableDirs["dirpool"] = "the directory does not exist" })
	if _, err := disks.Pool(ctx, "dirpool"); !hasCode(err, models.CodeStorageUnavailable) {
		t.Errorf("unusable directory pool = %v", err)
	}
	if pools, _ := disks.Pools(ctx); pools[1].Error == "" {
		t.Errorf("an unusable pool listed without its error: %+v", pools[1])
	}
}

func hasCode(err error, code string) bool {
	var me *models.Error
	return errors.As(err, &me) && me.Code == code
}
