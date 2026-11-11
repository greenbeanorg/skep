package hardware

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

func TestParseZpoolList(t *testing.T) {
	pools, err := parseZpoolList([]byte(mustRead(t, "testdata/freebsd/zpool-list.txt")))
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 2 {
		t.Fatalf("got %d pools", len(pools))
	}
	z := pools[0]
	if z.Name != "zroot" || z.SizeBytes != 127506841600 || z.AllocatedBytes != 3519680512 ||
		z.FreeBytes != 123987161088 || z.Health != "ONLINE" {
		t.Errorf("zroot = %+v", z)
	}
	if z.FragmentationPercent == nil || *z.FragmentationPercent != 1 ||
		z.CapacityPercent == nil || *z.CapacityPercent != 2 {
		t.Errorf("zroot percentages = %v %v", z.FragmentationPercent, z.CapacityPercent)
	}
	if pools[1].FragmentationPercent != nil {
		t.Errorf("a \"-\" fragmentation must be nil, got %d", *pools[1].FragmentationPercent)
	}
}

func TestParseZpoolListEmptyAndInvalid(t *testing.T) {
	pools, err := parseZpoolList(nil)
	if err != nil || pools == nil || len(pools) != 0 {
		t.Errorf("empty output: pools=%v err=%v; want empty non-nil slice", pools, err)
	}
	for _, bad := range []string{
		"zroot\t1\t2\n",
		"zroot\tbig\t2\t3\t1\t2\tONLINE\n",
		"zroot\t1\t2\t3\tx\t2\tONLINE\n",
		"no zpools available\n",
	} {
		if _, err := parseZpoolList([]byte(bad)); err == nil {
			t.Errorf("parseZpoolList(%q) succeeded, want error", bad)
		}
	}
}

func TestParseQEMUVersion(t *testing.T) {
	tests := map[string]string{
		mustRead(t, "testdata/qemu/system-version.txt"):             "9.2.0",
		mustRead(t, "testdata/qemu/img-version.txt"):                "9.2.0",
		"QEMU emulator version 10.0.2 (Debian 1:10.0.2+ds-2)\n":     "10.0.2",
		"qemu-img version 8.2 (some build)\nCopyright (c) 2003-...": "8.2",
	}
	for in, want := range tests {
		got, err := parseQEMUVersion([]byte(in))
		if err != nil || got != want {
			t.Errorf("parseQEMUVersion(%q) = %q, %v; want %q", firstLine([]byte(in)), got, err, want)
		}
	}
	for _, bad := range []string{"", "bhyve version 1.0", "version 9.2.0"} {
		if _, err := parseQEMUVersion([]byte(bad)); err == nil {
			t.Errorf("parseQEMUVersion(%q) succeeded, want error", bad)
		}
	}
}

func TestDiscoverQEMUStates(t *testing.T) {
	ctx := context.Background()

	none := discoverQEMU(ctx, fakeFinder(nil), &fakeRunner{})
	if none.Status != models.StatusUnavailable || none.Binaries == nil {
		t.Errorf("none = %+v", none)
	}

	imgOnly := discoverQEMU(ctx,
		fakeFinder(map[string]string{"qemu-img": "/usr/bin/qemu-img"}),
		&fakeRunner{outputs: map[string]string{"/usr/bin/qemu-img --version": "qemu-img version 9.2.0\n"}})
	if imgOnly.Status != models.StatusUnavailable || len(imgOnly.Binaries) != 1 || imgOnly.Acceleration != "" {
		t.Errorf("img only = %+v", imgOnly)
	}

	// A binary whose version cannot be read is still listed, with the error.
	broken := discoverQEMU(ctx,
		fakeFinder(map[string]string{"qemu-system-x86_64": "/usr/bin/qemu-system-x86_64"}),
		&fakeRunner{errs: map[string]error{"/usr/bin/qemu-system-x86_64 --version": errors.New("exit status 1")}})
	if broken.Status != models.StatusAvailable || broken.Binaries[0].Version != "" ||
		!strings.Contains(broken.Binaries[0].Error, "exit status 1") {
		t.Errorf("broken = %+v", broken)
	}
}

func TestDiscoverZFSPoolListFailure(t *testing.T) {
	info := discoverZFS(context.Background(),
		zfsKernelState{loaded: true, loadable: true, version: "2.4.2"},
		fakeFinder(map[string]string{"zpool": "/sbin/zpool"}),
		&fakeRunner{errs: map[string]error{
			"/sbin/zpool list -H -p -o name,size,allocated,free,fragmentation,capacity,health": errors.New("zpool: exit status 1"),
		}})
	if info.Status != models.StatusAvailable || info.PoolsError == "" || info.Pools == nil {
		t.Errorf("info = %+v", info)
	}

	noBinary := discoverZFS(context.Background(), zfsKernelState{loaded: true}, fakeFinder(nil), &fakeRunner{})
	if noBinary.Status != models.StatusUnavailable || !strings.Contains(noBinary.Reason, "zpool binary") {
		t.Errorf("no binary = %+v", noBinary)
	}
}

func TestFindInDirs(t *testing.T) {
	dir := t.TempDir()
	for name, mode := range map[string]os.FileMode{"tool": 0o755, "data": 0o644} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	find := findInDirs([]string{"/nonexistent-skep-dir", dir})

	if p, ok := find("tool"); !ok || p != filepath.Join(dir, "tool") {
		t.Errorf("find(tool) = %q, %v", p, ok)
	}
	for _, name := range []string{"data", "subdir", "missing", "", "../tool", "/bin/sh"} {
		if p, ok := find(name); ok {
			t.Errorf("find(%q) = %q, want not found", name, p)
		}
	}
}

func TestExecRunner(t *testing.T) {
	sh, ok := findInDirs(searchDirs)("sh")
	if !ok {
		t.Skip("no sh on this system")
	}
	r := execRunner{timeout: 5 * time.Second}
	ctx := context.Background()

	out, err := r.Run(ctx, sh, "-c", "echo hello; echo $LC_ALL")
	if err != nil || string(out) != "hello\nC\n" {
		t.Errorf("Run = %q, %v", out, err)
	}
	if _, err := r.Run(ctx, "sh", "-c", "true"); err == nil {
		t.Error("a relative command path must be refused")
	}
	if _, err := r.Run(ctx, sh, "-c", "echo oops >&2; exit 3"); err == nil || !strings.Contains(err.Error(), "oops") {
		t.Errorf("failure must carry stderr, got %v", err)
	}
	if _, err := (execRunner{timeout: 50 * time.Millisecond}).Run(ctx, sh, "-c", "sleep 5"); err == nil ||
		!strings.Contains(err.Error(), "timed out") {
		t.Errorf("want timeout error, got %v", err)
	}
}
