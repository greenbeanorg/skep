package hostops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/pkg/models"
)

// fakeMounts is a mount table; each unmount is also recorded in the
// script's calls, so its order against zfs and jail(8) can be checked.
type fakeMounts struct {
	table      []fsMount
	err        error
	unmountErr error
	unmounted  [][2]int32
	reads      int
	run        *script
}

func (m *fakeMounts) Mounts() ([]fsMount, error) {
	m.reads++
	return slices.Clone(m.table), m.err
}

func (m *fakeMounts) UnmountFSID(fsid [2]int32) error {
	if m.run != nil {
		m.run.calls = append(m.run.calls, "unmount "+fsidString(fsid))
	}
	if m.unmountErr != nil {
		return m.unmountErr
	}
	m.unmounted = append(m.unmounted, fsid)
	return nil
}

func jailNotRunning(run *script) {
	run.answers["jls -j skep-"+ctrA+" jid"] = &CommandError{Name: "jls", ExitCode: 1,
		Stderr: "jls: jail \"skep-" + ctrA + "\" not found", Err: errors.New("exit status 1")}
}

// TestRemoveLeftoverDevfsExactMatchOnly: only a devfs whose f_mntonname
// is exactly <root>/dev goes, by fsid, newest first; the host's /dev, a
// devfs elsewhere, another file system on <root>/dev, and another jail's
// devfs stay (amendment l).
func TestRemoveLeftoverDevfsExactMatchOnly(t *testing.T) {
	h, _, mp := jailHost(t)
	root := filepath.Join(mp, ctrA)
	other := filepath.Join(mp, ctrB)
	m := &fakeMounts{table: []fsMount{
		{On: "/", FSType: "ufs", FSID: [2]int32{1, 1}},
		{On: "/dev", FSType: "devfs", FSID: [2]int32{2, 2}},
		{On: root, FSType: "zfs", FSID: [2]int32{3, 3}},
		{On: root + "/dev", FSType: "devfs", FSID: [2]int32{4, 4}},
		{On: root + "/dev/fd", FSType: "fdescfs", FSID: [2]int32{5, 5}},
		{On: root + "/devx", FSType: "devfs", FSID: [2]int32{6, 6}},
		{On: root + "/dev/", FSType: "devfs", FSID: [2]int32{7, 7}},
		{On: root + "/DEV", FSType: "devfs", FSID: [2]int32{8, 8}},
		{On: root + "/usr/dev", FSType: "devfs", FSID: [2]int32{9, 9}},
		{On: other + "/dev", FSType: "devfs", FSID: [2]int32{10, 10}},
		{On: root + "/dev", FSType: "nullfs", FSID: [2]int32{11, 11}},
		{On: root + "/dev", FSType: "devfs", FSID: [2]int32{12, 12}},
	}}
	h.mounts = m
	got, err := h.removeLeftoverDevfs(ctrA, root)
	if err != nil {
		t.Fatal(err)
	}
	if want := [][2]int32{{12, 12}, {4, 4}}; !slices.Equal(m.unmounted, want) {
		t.Fatalf("unmounted %v, want %v", m.unmounted, want)
	}
	if !slices.Equal(got, []string{"FSID:12:12", "FSID:4:4"}) {
		t.Fatalf("returned %q", got)
	}
}

// TestRemoveLeftoverDevfsSymlinkedDevLeavesHostAlone: a jail root whose
// dev is a symbolic link to the host's /dev, or to a directory that has a
// devfs, unmounts nothing. The match is on the mount table's own text,
// never a resolved path, so the link is never followed (amendment l).
func TestRemoveLeftoverDevfsSymlinkedDevLeavesHostAlone(t *testing.T) {
	h, run, mp := jailHost(t)
	jailNotRunning(run)
	root := filepath.Join(mp, ctrA)
	elsewhere := filepath.Join(t.TempDir(), "devfs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/dev", elsewhere} {
		os.Remove(filepath.Join(root, "dev"))
		if err := os.Symlink(target, filepath.Join(root, "dev")); err != nil {
			t.Fatal(err)
		}
		m := &fakeMounts{table: []fsMount{
			{On: "/dev", FSType: "devfs", FSID: [2]int32{2, 2}},
			{On: elsewhere, FSType: "devfs", FSID: [2]int32{3, 3}},
		}}
		h.mounts = m
		got, err := h.RemoveLeftoverDevfs(context.Background(), "skeptest", ctrA)
		if err != nil || len(got) != 0 || len(m.unmounted) != 0 {
			t.Fatalf("dev -> %s: unmounted %v (returned %q, %v); want nothing", target, m.unmounted, got, err)
		}
		if m.reads != 1 {
			t.Fatalf("mount table read %d times", m.reads)
		}
	}
}

func TestRemoveLeftoverDevfsRefusals(t *testing.T) {
	h, _, mp := jailHost(t)
	root := filepath.Join(mp, ctrA)

	m := &fakeMounts{table: []fsMount{{On: root + "/dev", FSType: "devfs"}}}
	h.mounts = m
	if _, err := h.removeLeftoverDevfs(ctrA, root); err == nil || len(m.unmounted) != 0 {
		t.Fatalf("a devfs without an fsid: %v, unmounted %v", err, m.unmounted)
	}

	m = &fakeMounts{table: []fsMount{{On: root + "/dev", FSType: "devfs", FSID: [2]int32{4, 4}}}, unmountErr: errors.New("EBUSY")}
	h.mounts = m
	if _, err := h.removeLeftoverDevfs(ctrA, root); err == nil || !strings.Contains(err.Error(), "EBUSY") {
		t.Fatalf("an unmount failure: %v", err)
	}

	h.mounts = &fakeMounts{err: errors.New("no table")}
	if _, err := h.removeLeftoverDevfs(ctrA, root); err == nil {
		t.Fatal("a mount table that could not be read was ignored")
	}

	for _, bad := range []string{"relative/root", root + "/../" + ctrA} {
		m = &fakeMounts{}
		h.mounts = m
		if _, err := h.removeLeftoverDevfs(ctrA, bad); err == nil || m.reads != 0 {
			t.Fatalf("root %q: %v (reads %d)", bad, err, m.reads)
		}
	}
}

func TestRemoveLeftoverDevfsRefusesRunning(t *testing.T) {
	h, run, mp := jailHost(t)
	run.answers["jls -j skep-"+ctrA+" jid"] = "12\n"
	m := &fakeMounts{table: []fsMount{{On: filepath.Join(mp, ctrA, "dev"), FSType: "devfs", FSID: [2]int32{4, 4}}}}
	h.mounts = m
	_, err := h.RemoveLeftoverDevfs(context.Background(), "skeptest", ctrA)
	wantCode(t, err, models.CodeContainerNotStopped)
	if m.reads != 0 || len(m.unmounted) != 0 {
		t.Fatalf("a running jail's devfs was looked at (%d reads) or unmounted (%v)", m.reads, m.unmounted)
	}
}

// TestDestroyJailRootUnmountsLeftoverDevfsFirst: the leftover devfs goes
// before zfs destroy, which it would make fail with "dataset is busy".
func TestDestroyJailRootUnmountsLeftoverDevfsFirst(t *testing.T) {
	ds := "skeptest/skep/jail/" + ctrA
	f := make([]string, datasetFields)
	for i := range f {
		f[i] = "-"
	}
	f[0], f[1], f[17] = ds, "filesystem", ctrA
	h, run, mp := jailHost(t)
	h.sys.(fakeSystem).oids[sysctlNCPU] = 4
	jailNotRunning(run)
	run.answers["zfs list -H -p -t filesystem,volume -o "+datasetColumns+" -r "+ds] = strings.Join(f, "\t") + "\n"
	m := &fakeMounts{run: run, table: []fsMount{
		{On: "/dev", FSType: "devfs", FSID: [2]int32{2, 2}},
		{On: filepath.Join(mp, ctrA, "dev"), FSType: "devfs", FSID: [2]int32{4, 4}},
	}}
	h.mounts = m
	if _, err := h.DestroyJailRoot(context.Background(), "skeptest", ctrA); err != nil {
		t.Fatal(err)
	}
	u := slices.Index(run.calls, "unmount FSID:4:4")
	d := slices.IndexFunc(run.calls, func(c string) bool { return strings.HasPrefix(c, "zfs destroy") })
	if u < 0 || d < 0 || u > d {
		t.Fatalf("want the unmount before zfs destroy: %q", run.calls)
	}
	if !slices.Equal(m.unmounted, [][2]int32{{4, 4}}) {
		t.Fatalf("unmounted %v", m.unmounted)
	}
}

// TestDestroyJailRootUnmountFailureKeepsDataset: if the leftover devfs
// cannot be unmounted, zfs destroy does not run.
func TestDestroyJailRootUnmountFailureKeepsDataset(t *testing.T) {
	ds := "skeptest/skep/jail/" + ctrA
	f := make([]string, datasetFields)
	for i := range f {
		f[i] = "-"
	}
	f[0], f[1], f[17] = ds, "filesystem", ctrA
	h, run, mp := jailHost(t)
	h.sys.(fakeSystem).oids[sysctlNCPU] = 4
	jailNotRunning(run)
	run.answers["zfs list -H -p -t filesystem,volume -o "+datasetColumns+" -r "+ds] = strings.Join(f, "\t") + "\n"
	h.mounts = &fakeMounts{unmountErr: errors.New("EBUSY"),
		table: []fsMount{{On: filepath.Join(mp, ctrA, "dev"), FSType: "devfs", FSID: [2]int32{4, 4}}}}
	if _, err := h.DestroyJailRoot(context.Background(), "skeptest", ctrA); err == nil {
		t.Fatal("the delete went on although the devfs stayed")
	}
	if len(callsWith(run.calls, "zfs destroy")) != 0 {
		t.Fatalf("zfs destroy ran: %q", run.calls)
	}
}
