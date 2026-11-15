package hostops

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/pkg/models"
)

// fakeStatter answers StatDir for the pool directory from a template,
// reading the real type bits so a planted symbolic link shows.
type fakeStatter struct {
	st  DirStat
	err error
}

func (f *fakeStatter) StatDir(path string) (DirStat, error) {
	if f.err != nil {
		return DirStat{}, f.err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return DirStat{}, err
	}
	st := f.st
	st.Mode = fi.Mode().Type() | f.st.Mode.Perm()
	return st, nil
}

const (
	testVM   = "11111111-2222-4333-8444-555555555555"
	testDisk = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
)

func testDirs(t *testing.T) (*dirs, *fakeStatter, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dirpool")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	conf := testConf(t)
	conf.Directories = []DirPool{{Name: "dirpool", Path: dir}}
	stat := &fakeStatter{st: DirStat{Mode: 0o700, UID: 0, FSType: "ufs",
		SizeBytes: 100 << 30, FreeBytes: 60 << 30, AvailableBytes: 50 << 30}}
	return newDirs(conf, stat, slog.New(slog.NewTextHandler(io.Discard, nil))), stat, dir
}

func diskSpec(slot int, size int64, thin bool) VolumeSpec {
	return VolumeSpec{Pool: "dirpool", VMID: testVM, DiskID: testDisk, Slot: slot, SizeBytes: size, Thin: thin}
}

func TestDirPoolChecks(t *testing.T) {
	ctx := context.Background()
	d, stat, dir := testDirs(t)
	p, err := d.Pool(ctx, "dirpool")
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != PoolKindDirectory || p.Health != "ONLINE" || p.SizeBytes != 100<<30 ||
		p.AvailableBytes != 50<<30 || p.AllocatedBytes != 40<<30 {
		t.Errorf("pool = %+v", p)
	}
	_, err = d.Pool(ctx, "skeptest")
	wantCode(t, err, models.CodePoolNotAllowed)

	for name, change := range map[string]func(){
		"group readable": func() { stat.st.Mode = 0o750 },
		"not root":       func() { stat.st.UID = 1001 },
		"nfs":            func() { stat.st.FSType = "nfs" },
		"tmpfs":          func() { stat.st.FSType = "tmpfs" },
		"missing":        func() { stat.err = fs.ErrNotExist },
	} {
		saved := stat.st
		change()
		_, err := d.Pool(ctx, "dirpool")
		wantCode(t, err, models.CodeStorageUnavailable)
		list, lerr := d.Pools(ctx)
		if lerr != nil || len(list) != 1 || list[0].Error == "" || list[0].Health != "" {
			t.Errorf("%s: Pools = %+v, %v", name, list, lerr)
		}
		// Nothing is created in a pool that cannot be used.
		if _, err := d.CreateVMDir(ctx, "dirpool", testVM); err == nil {
			t.Errorf("%s: CreateVMDir succeeded", name)
		}
		stat.st, stat.err = saved, nil
	}

	// The configured path being a symbolic link is refused, wherever it
	// points.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	d.conf.Directories[0].Path = link
	_, err = d.Pool(ctx, "dirpool")
	wantCode(t, err, models.CodeStorageUnavailable)
}

func TestDirPoolDiskLifecycle(t *testing.T) {
	ctx := context.Background()
	d, _, dir := testDirs(t)
	if _, err := d.CreateVMDir(ctx, "dirpool", testVM); err != nil {
		t.Fatal(err)
	}
	vmDir := filepath.Join(dir, "skep", "vm", testVM)
	if fi, err := os.Stat(vmDir); err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("VM directory: %v %v", fi, err)
	}
	// Creating the directory again is not an error.
	if _, err := d.CreateVMDir(ctx, "dirpool", testVM); err != nil {
		t.Fatal(err)
	}

	thin, err := d.CreateFile(ctx, diskSpec(0, 1<<30, true))
	if err != nil {
		t.Fatal(err)
	}
	if thin.Type != TypeFile || thin.VolSizeBytes != 1<<30 || thin.Name != filepath.Join(vmDir, "disk0.img") ||
		thin.UsedBytes >= 1<<30 {
		t.Errorf("thin disk = %+v", thin)
	}
	if fi, _ := os.Stat(thin.Name); fi.Mode().Perm() != 0o600 {
		t.Errorf("disk file mode = %v", fi.Mode())
	}
	if _, err := d.CreateFile(ctx, diskSpec(0, 1<<30, true)); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Errorf("a second create of slot 0 = %v", err)
	}

	full, err := d.CreateFile(ctx, diskSpec(1, 2<<20, false))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(full.Name)
	if int64(len(data)) != 2<<20 || !bytes.Equal(data, make([]byte, 2<<20)) {
		t.Errorf("preallocated disk: %d bytes", len(data))
	}

	grown, err := d.ResizeFile(ctx, diskSpec(0, 2<<30, true))
	if err != nil || grown.VolSizeBytes != 2<<30 {
		t.Errorf("grow = %+v, %v", grown, err)
	}
	if _, err := d.ResizeFile(ctx, diskSpec(0, 1<<30, true)); err == nil {
		t.Error("a shrink was accepted")
	}
	wantCode(t, mustErr(d.ResizeFile(ctx, diskSpec(0, 2<<30, true))), models.CodeInvalidRequest)
	wantCode(t, mustErr(d.ResizeFile(ctx, diskSpec(0, 2<<30+100, true))), models.CodeInvalidRequest)
	full, err = d.ResizeFile(ctx, diskSpec(1, 3<<20, false))
	if err != nil || full.VolSizeBytes != 3<<20 {
		t.Errorf("grow preallocated = %+v, %v", full, err)
	}

	list, err := d.VMFiles(ctx, "dirpool", testVM)
	if err != nil || len(list) != 2 || list[0].VolSizeBytes != 2<<30 || list[1].VolSizeBytes != 3<<20 {
		t.Errorf("VMFiles = %+v, %v", list, err)
	}

	gone, err := d.DestroyFile(ctx, diskSpec(1, 0, false))
	if err != nil || gone.Name != full.Name {
		t.Errorf("destroy = %+v, %v", gone, err)
	}
	if err := mustErr(d.DestroyFile(ctx, diskSpec(1, 0, false))); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("destroying a missing file = %v", err)
	}

	all, err := d.DestroyVM(ctx, "dirpool", testVM)
	if err != nil || len(all) != 2 {
		t.Errorf("DestroyVM = %+v, %v", all, err)
	}
	if _, err := os.Stat(vmDir); !os.IsNotExist(err) {
		t.Errorf("the VM directory is still there: %v", err)
	}
	all, err = d.DestroyVM(ctx, "dirpool", testVM)
	if err != nil || len(all) != 0 {
		t.Errorf("DestroyVM of nothing = %+v, %v", all, err)
	}
}

func mustErr(_ Dataset, err error) error { return err }

func TestDirPoolSpaceAndCancel(t *testing.T) {
	ctx := context.Background()
	d, stat, dir := testDirs(t)
	if _, err := d.CreateVMDir(ctx, "dirpool", testVM); err != nil {
		t.Fatal(err)
	}
	disk0 := filepath.Join(dir, "skep", "vm", testVM, "disk0.img")

	stat.st.AvailableBytes = 1 << 20
	wantCode(t, mustErr(d.CreateFile(ctx, diskSpec(0, 2<<20, false))), models.CodeInsufficientSpace)
	// A thin disk does not need the space now.
	if _, err := d.CreateFile(ctx, diskSpec(0, 2<<20, true)); err != nil {
		t.Fatal(err)
	}
	wantCode(t, mustErr(d.ResizeFile(ctx, diskSpec(0, 4<<20, false))), models.CodeInsufficientSpace)
	if fi, _ := os.Stat(disk0); fi.Size() != 2<<20 {
		t.Errorf("a refused growth changed the size to %d", fi.Size())
	}
	stat.st.AvailableBytes = 50 << 30

	// A cancelled preallocation leaves no file behind.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := d.CreateFile(cancelled, diskSpec(1, 4<<20, false)); err == nil {
		t.Fatal("a cancelled preallocation succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "skep", "vm", testVM, "disk1.img")); !os.IsNotExist(err) {
		t.Errorf("a cancelled create left its file: %v", err)
	}
	// A cancelled growth leaves the old size.
	if _, err := d.ResizeFile(cancelled, diskSpec(0, 8<<20, false)); err == nil {
		t.Fatal("a cancelled growth succeeded")
	}
	if fi, _ := os.Stat(disk0); fi.Size() != 2<<20 {
		t.Errorf("a cancelled growth left the size at %d", fi.Size())
	}
}

func TestDirPoolRefusesWhatSkepDidNotMake(t *testing.T) {
	ctx := context.Background()
	d, _, dir := testDirs(t)
	if _, err := d.CreateVMDir(ctx, "dirpool", testVM); err != nil {
		t.Fatal(err)
	}
	vmDir := filepath.Join(dir, "skep", "vm", testVM)
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A disk file that is a symbolic link is never followed.
	if err := os.Symlink(outside, filepath.Join(vmDir, "disk0.img")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ResizeFile(ctx, diskSpec(0, 1<<30, true)); err == nil {
		t.Error("growing a symbolic link was accepted")
	}
	if _, err := d.DestroyFile(ctx, diskSpec(0, 0, true)); err == nil {
		t.Error("destroying a symbolic link was accepted")
	}
	if _, err := d.CreateFile(ctx, diskSpec(0, 1<<20, true)); err == nil {
		t.Error("creating over a symbolic link was accepted")
	}
	if data, _ := os.ReadFile(outside); string(data) != "keep" {
		t.Errorf("the file outside the pool changed: %q", data)
	}
	// DestroyVM refuses a directory with anything it did not make, and
	// removes nothing.
	if _, err := d.DestroyVM(ctx, "dirpool", testVM); err == nil {
		t.Error("DestroyVM removed a directory holding a symbolic link")
	}
	os.Remove(filepath.Join(vmDir, "disk0.img"))
	if err := os.WriteFile(filepath.Join(vmDir, "notes.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateFile(ctx, diskSpec(1, 1<<20, true)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DestroyVM(ctx, "dirpool", testVM); err == nil {
		t.Error("DestroyVM removed a directory holding a foreign file")
	}
	if _, err := os.Stat(filepath.Join(vmDir, "disk1.img")); err != nil {
		t.Errorf("a refused DestroyVM removed a disk: %v", err)
	}

	// A VM directory planted as a link out of the pool does not escape it.
	other := "22222222-2222-4333-8444-555555555555"
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "skep", "vm", other)); err != nil {
		t.Fatal(err)
	}
	spec := diskSpec(0, 1<<20, true)
	spec.VMID = other
	if _, err := d.CreateFile(ctx, spec); err == nil {
		t.Error("a disk was created through a link out of the pool")
	}
}

func TestDiskPath(t *testing.T) {
	d, _, dir := testDirs(t)
	got, err := d.conf.DiskPath("dirpool", testVM, 2)
	if err != nil || got != filepath.Join(dir, "skep", "vm", testVM, "disk2.img") {
		t.Errorf("directory disk path = %q, %v", got, err)
	}
	got, err = d.conf.DiskPath("skeptest", testVM, 0)
	if err != nil || got != "/dev/zvol/skeptest/skep/vm/"+testVM+"/disk0" {
		t.Errorf("zvol path = %q, %v", got, err)
	}
	_, err = d.conf.DiskPath("tank", testVM, 0)
	wantCode(t, err, models.CodePoolNotAllowed)
	if _, err := d.conf.DiskPath("dirpool", "../../etc", 0); err == nil {
		t.Error("an invalid VM ID was accepted")
	}
	if _, err := d.conf.DiskPath("dirpool", testVM, 32); err == nil {
		t.Error("slot 32 was accepted")
	}
	if !d.conf.IsDirectory("dirpool") || d.conf.IsDirectory("skeptest") {
		t.Error("IsDirectory")
	}
}
