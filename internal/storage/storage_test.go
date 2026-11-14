package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/hostops/hostfake"
	"github.com/greenbeanorg/skep/pkg/models"
)

const (
	vmA    = "0b0b6f4e-5d0c-4a52-9a43-0d2f8a1f3c11"
	vmGone = "1c1c6f4e-5d0c-4a52-9a43-0d2f8a1f3c22"
	diskA  = "7e3c1a90-1111-4222-8333-444455556666"
	snapA  = "11111111-aaaa-4bbb-8ccc-000000000001"
)

type owners database.Owners

func (o owners) Owners(context.Context) (database.Owners, error) { return database.Owners(o), nil }

func setup(t *testing.T, pools ...string) (*Service, *hostfake.Fake) {
	t.Helper()
	dir := t.TempDir()
	conf := hostops.Confinement{Pools: pools, DatasetRoot: "skep",
		RunDir: filepath.Join(dir, "run"), StateDir: filepath.Join(dir, "state")}
	f := hostfake.New(conf, hostfake.Options{})
	s, err := New(Options{Host: f.Ops(), Owners: owners{VMs: map[string]bool{vmA: true}}})
	if err != nil {
		t.Fatal(err)
	}
	return s, f
}

func TestInventoryClassifiesOwners(t *testing.T) {
	s, f := setup(t, "skeptest")
	ctx := context.Background()
	z := f.Ops().ZFS
	f.AddDataset(hostops.Dataset{Name: "skeptest/skep", Type: "filesystem"})
	f.AddDataset(hostops.Dataset{Name: "skeptest/skep/vm", Type: "filesystem"})
	if _, err := z.CreateVMDataset(ctx, "skeptest", vmA); err != nil {
		t.Fatal(err)
	}
	if _, err := z.CreateVolume(ctx, hostops.VolumeSpec{Pool: "skeptest", VMID: vmA, DiskID: diskA, Slot: 0, SizeBytes: 1 << 30, BlockSize: 16384}); err != nil {
		t.Fatal(err)
	}
	if _, err := z.Snapshot(ctx, "skeptest", hostops.Owner{Kind: hostops.OwnerVM, ID: vmA}, snapA); err != nil {
		t.Fatal(err)
	}
	// Hand-made and leftover datasets.
	f.AddDataset(hostops.Dataset{Name: "skeptest/skep/scratch", Type: "filesystem"})
	f.AddDataset(hostops.Dataset{Name: "skeptest/skep/vm/" + vmGone, Type: "filesystem", VMID: vmGone})
	f.AddDataset(hostops.Dataset{Name: "skeptest/skep/vm/notanid", Type: "filesystem"})
	f.AddDataset(hostops.Dataset{Name: "skeptest/skep/vm/" + vmA + "/extra", Type: "filesystem", VMID: vmA})

	list, err := s.Datasets(ctx, models.DatasetListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]models.DatasetOwner{}
	for _, d := range list.Items {
		got[d.Name] = d.Owner
		if d.Pool != "skeptest" {
			t.Errorf("%s: pool %q", d.Name, d.Pool)
		}
	}
	vmDS := "skeptest/skep/vm/" + vmA
	want := map[string]string{
		"skeptest/skep":               models.DatasetOwnerSkep,
		"skeptest/skep/vm":            models.DatasetOwnerSkep,
		vmDS:                          models.DatasetOwnerVM,
		vmDS + "/disk0":               models.DatasetOwnerVM,
		vmDS + "@skep-" + snapA:       models.DatasetOwnerVM,
		vmDS + "/disk0@skep-" + snapA: models.DatasetOwnerVM,
		vmDS + "/extra":               models.DatasetOwnerUnowned,
		"skeptest/skep/scratch":       models.DatasetOwnerUnowned,
		"skeptest/skep/vm/" + vmGone:  models.DatasetOwnerUnowned,
		"skeptest/skep/vm/notanid":    models.DatasetOwnerUnowned,
	}
	if len(got) != len(want) {
		t.Errorf("listed %d datasets, want %d: %v", len(got), len(want), got)
	}
	for name, kind := range want {
		o := got[name]
		if o.Kind != kind {
			t.Errorf("%s: owner %+v, want %s", name, o, kind)
		}
		if kind == models.DatasetOwnerUnowned && o.Reason == "" {
			t.Errorf("%s: unowned without a reason", name)
		}
		if kind == models.DatasetOwnerVM && o.ID != vmA {
			t.Errorf("%s: owner ID %q", name, o.ID)
		}
	}
	for _, d := range list.Items {
		if d.Name == vmDS+"/disk0@skep-"+snapA && d.SnapshotID != snapA {
			t.Errorf("snapshot ID = %q", d.SnapshotID)
		}
		if d.Name == vmDS+"/disk0" && (d.VolSizeBytes != 1<<30 || d.VolBlockSize != 16384) {
			t.Errorf("volume = %+v", d)
		}
	}
	// Listing changed nothing.
	if calls := f.Calls(); len(calls) != 3 {
		t.Errorf("calls = %v", calls)
	}
}

func TestInventoryPages(t *testing.T) {
	s, f := setup(t, "p1", "p2")
	for _, name := range []string{"p1/skep", "p1/skep/a", "p1/skep/b", "p2/skep", "p2/skep/c"} {
		f.AddDataset(hostops.Dataset{Name: name, Type: "filesystem"})
	}
	ctx := context.Background()
	var names []string
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		list, err := s.Datasets(ctx, models.DatasetListOptions{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range list.Items {
			names = append(names, d.Name)
		}
		if cursor = list.NextCursor; cursor == "" {
			break
		}
	}
	if want := "p1/skep p1/skep/a p1/skep/b p2/skep p2/skep/c"; join(names) != want {
		t.Errorf("pages = %v", names)
	}

	// One pool alone; a pool outside the allow-list; bad limits and cursors.
	list, err := s.Datasets(ctx, models.DatasetListOptions{Pool: "p2"})
	if err != nil || len(list.Items) != 2 {
		t.Errorf("p2 = %+v, %v", list, err)
	}
	for name, opts := range map[string]models.DatasetListOptions{
		"pool":           {Pool: "zroot"},
		"limit":          {Limit: MaxPageSize + 1},
		"cursor":         {Cursor: "!!"},
		"foreign cursor": {Cursor: "em9vdC94"}, // "zoot/x"
	} {
		if _, err := s.Datasets(ctx, opts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// Without ZFS, every pool is unavailable, which is reported, not fatal.
	f.Set(func(f *hostfake.Fake) { f.ZFSLoaded = false })
	list, err = s.Datasets(ctx, models.DatasetListOptions{})
	if err != nil || len(list.Items) != 0 || len(list.UnavailablePools) != 2 {
		t.Errorf("without zfs: %+v, %v", list, err)
	}
	_, err = s.Datasets(ctx, models.DatasetListOptions{Pool: "p1"})
	var me *models.Error
	if !errors.As(err, &me) || me.Code != models.CodeStorageUnavailable {
		t.Errorf("one pool without zfs: %v", err)
	}
}

func join(list []string) string {
	out := ""
	for i, s := range list {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}
