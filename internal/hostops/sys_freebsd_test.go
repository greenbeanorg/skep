//go:build freebsd

package hostops

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestStatDirNative reads a real directory with the FreeBSD binding: the
// type bits come from lstat(2), so a link is a link, and statfs(2) names
// the file system and its sizes.
func TestStatDirNative(t *testing.T) {
	dir := t.TempDir()
	st, err := kernel{}.StatDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Mode.IsDir() || st.UID != uint32(os.Getuid()) || st.FSType == "" ||
		st.SizeBytes <= 0 || st.FreeBytes <= 0 || st.AvailableBytes > st.FreeBytes {
		t.Errorf("StatDir(%s) = %+v", dir, st)
	}
	t.Logf("StatDir(%s): fstype %s, size %d, free %d, available %d", dir, st.FSType, st.SizeBytes, st.FreeBytes, st.AvailableBytes)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if st, err := (kernel{}).StatDir(link); err != nil || st.Mode&os.ModeSymlink == 0 || st.Mode.IsDir() {
		t.Errorf("StatDir(link) = %+v, %v", st, err)
	}
	if _, err := (kernel{}).StatDir(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Errorf("StatDir(missing) = %v", err)
	}
}

// TestDirPoolNative runs the directory pool against the real binding on
// a directory the test user owns. The owner check is root's, so it is
// relaxed to this user; everything else (statfs type, mode, sparse files
// on this file system) is the host's.
func TestDirPoolNative(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pool")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	conf := Confinement{DatasetRoot: "skep", Directories: []DirPool{{Name: "native", Path: dir}}}
	d := newDirs(conf, ownerAsRoot{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	p, err := d.Pool(ctx, "native")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pool: %+v", p)
	if _, err := d.CreateVMDir(ctx, "native", testVM); err != nil {
		t.Fatal(err)
	}
	spec := VolumeSpec{Pool: "native", VMID: testVM, DiskID: testDisk, Slot: 0, SizeBytes: 1 << 30, Thin: true}
	thin, err := d.CreateFile(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("thin 1 GiB file: size %d, allocated %d", thin.VolSizeBytes, thin.UsedBytes)
	if thin.VolSizeBytes != 1<<30 || thin.UsedBytes >= 1<<20 {
		t.Errorf("a thin file is not sparse here: %+v", thin)
	}
	spec.Slot, spec.SizeBytes, spec.Thin = 1, 8<<20, false
	full, err := d.CreateFile(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("preallocated 8 MiB file: size %d, allocated %d", full.VolSizeBytes, full.UsedBytes)
	if full.UsedBytes < 8<<20 {
		t.Errorf("a preallocated file is not allocated here: %+v", full)
	}
	gone, err := d.DestroyVM(ctx, "native", testVM)
	if err != nil || len(gone) != 3 {
		t.Errorf("DestroyVM = %+v, %v", gone, err)
	}
}

// ownerAsRoot is the FreeBSD binding with the owner reported as root.
type ownerAsRoot struct{}

func (ownerAsRoot) StatDir(path string) (DirStat, error) {
	st, err := kernel{}.StatDir(path)
	if err == nil && st.UID == uint32(os.Getuid()) {
		st.UID = 0
	}
	return st, err
}
