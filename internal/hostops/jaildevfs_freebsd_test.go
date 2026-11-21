//go:build freebsd

package hostops

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// recordingMounts reads the real mount table but never unmounts: any
// unmount it is asked for fails the test (amendment l).
type recordingMounts struct {
	t     *testing.T
	asked []string
}

func (r *recordingMounts) Mounts() ([]fsMount, error) { return kernelMounts{}.Mounts() }

func (r *recordingMounts) UnmountFSID(fsid [2]int32) error {
	r.asked = append(r.asked, fsidString(fsid))
	r.t.Errorf("asked to unmount %s", fsidString(fsid))
	return nil
}

func devfsOnDev(t *testing.T, ms []fsMount) fsMount {
	t.Helper()
	for _, m := range ms {
		if m.On == "/dev" && m.FSType == "devfs" {
			return m
		}
	}
	t.Fatalf("no devfs on /dev in the mount table")
	return fsMount{}
}

// TestNativeMountTable reads the real mount table with getfsstat(2): the
// host's /dev is a devfs and, as root, has an fsid. Read-only.
func TestNativeMountTable(t *testing.T) {
	ms, err := kernelMounts{}.Mounts()
	if err != nil {
		t.Fatal(err)
	}
	dev := devfsOnDev(t, ms)
	if os.Geteuid() == 0 && dev.FSID == ([2]int32{}) {
		t.Fatal("root sees no fsid for /dev")
	}
	for _, m := range ms {
		if m.FSType == "devfs" {
			t.Logf("devfs on %s %s", m.On, fsidString(m.FSID))
		}
	}
}

// TestNativeSymlinkedDevLeavesHostUntouched: a jail root whose dev is a
// symbolic link to the host's /dev (or to /) finds nothing to unmount in
// the real mount table, and the host's /dev keeps its fsid. Nothing is
// unmounted: the unmount is replaced by one that fails the test.
func TestNativeSymlinkedDevLeavesHostUntouched(t *testing.T) {
	before, err := kernelMounts{}.Mounts()
	if err != nil {
		t.Fatal(err)
	}
	dev := devfsOnDev(t, before)
	h := newHost(Confinement{}, nil, kernel{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	for _, target := range []string{"/dev", "/"} {
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, "dev")); err != nil {
			t.Fatal(err)
		}
		rec := &recordingMounts{t: t}
		h.mounts = rec
		got, err := h.removeLeftoverDevfs(ctrA, root)
		if err != nil || len(got) != 0 || len(rec.asked) != 0 {
			t.Fatalf("dev -> %s: %q, %v, asked %q", target, got, err, rec.asked)
		}
		t.Logf("root %s, dev -> %s: nothing to unmount", root, target)
	}
	after, err := kernelMounts{}.Mounts()
	if err != nil {
		t.Fatal(err)
	}
	if devfsOnDev(t, after) != dev || !slices.Equal(before, after) {
		t.Fatal("the mount table changed")
	}
}
