package hostops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/pkg/models"
)

const (
	snap1  = "11111111-aaaa-4bbb-8ccc-000000000001"
	snap2  = "11111111-aaaa-4bbb-8ccc-000000000002"
	snap3  = "11111111-aaaa-4bbb-8ccc-000000000003"
	diskB  = "33333333-aaaa-4bbb-8ccc-000000000003"
	diskA1 = "7e3c1a90-1111-4222-8333-444455556667"
)

// snapLine is a snapshot line: skep:snapshot_id given, and the VM's
// properties inherited from the dataset, as zfs list shows them.
func snapLine(dataset, short, snapshotID, vmID string, txg int, clones string) string {
	kv := []string{"used", "0", "referenced", "57344", "createtxg", strconv.Itoa(txg), "skep:vm_id", vmID, "skep:snapshot_id", snapshotID}
	if clones != "" {
		kv = append(kv, "clones", clones)
	}
	return zline(dataset+"@"+short, "snapshot", kv...)
}

// vmTree sets up a VM with two disks and answers its listings.
func vmTree(t *testing.T, snaps string) (*host, *script, string) {
	t.Helper()
	ds := "skeptest/skep/vm/" + vmA
	run := &script{answers: map[string]any{
		"zfs list -H -p -t filesystem,volume -o " + listCols + " -r " + ds: fsys(ds, vmA, 10) + vol(ds+"/disk0", vmA, diskA, 11) + vol(ds+"/disk1", vmA, diskA1, 12),
		"zfs list -H -p -t snapshot -o " + listCols + " -r " + ds:          snaps,
	}}
	return testHost(t, run, true), run, ds
}

// threeSnaps is snap1, snap2, snap3 of the whole tree, in that order.
func threeSnaps(ds string, clonesOf2 string) string {
	var b strings.Builder
	for i, id := range []string{snap1, snap2, snap3} {
		for _, d := range []string{ds, ds + "/disk0", ds + "/disk1"} {
			clones := ""
			if id == snap2 && d == ds+"/disk0" {
				clones = clonesOf2
			}
			b.WriteString(snapLine(d, "skep-"+id, id, vmA, 100+10*i, clones))
		}
	}
	return b.String()
}

func mutations(calls []string) []string {
	var out []string
	for _, c := range calls {
		if !strings.HasPrefix(c, "zfs list") {
			out = append(out, c)
		}
	}
	return out
}

func TestSnapshotIsOneRecursiveCommand(t *testing.T) {
	h, run, ds := vmTree(t, threeSnaps("skeptest/skep/vm/"+vmA, "")[:0])
	run.answers["zfs list -H -p -t snapshot -o "+listCols+" -r "+ds] =
		snapLine(ds, "skep-"+snap1, snap1, vmA, 100, "") + snapLine(ds+"/disk0", "skep-"+snap1, snap1, vmA, 100, "") +
			snapLine(ds+"/disk1", "skep-"+snap1, snap1, vmA, 100, "")
	got, err := h.Snapshot(context.Background(), "skeptest", Owner{Kind: OwnerVM, ID: vmA}, snap1)
	if err != nil || len(got) != 3 {
		t.Fatalf("snapshot = %+v, %v", got, err)
	}
	want := []string{"zfs snapshot -r -o skep:snapshot_id=" + snap1 + " " + ds + "@skep-" + snap1}
	if m := mutations(run.calls); !reflect.DeepEqual(m, want) {
		t.Errorf("mutations = %v\nwant %v", m, want)
	}

	// Nothing from a request becomes part of a name.
	for _, bad := range []string{"", "x", "a@b", snap1 + "/..", strings.ToUpper(snap1)} {
		if _, err := h.Snapshot(context.Background(), "skeptest", Owner{Kind: OwnerVM, ID: vmA}, bad); err == nil {
			t.Errorf("snapshot ID %q accepted", bad)
		}
	}
	if _, err := h.Snapshot(context.Background(), "zroot", Owner{Kind: OwnerVM, ID: vmA}, snap1); err == nil {
		t.Error("a pool outside the allow-list was accepted")
	}
	if _, err := h.Snapshot(context.Background(), "skeptest", Owner{Kind: "disk", ID: vmA}, snap1); err == nil {
		t.Error("an unknown owner kind was accepted")
	}
}

func TestSnapshotRefusesLockedAndForeignDatasets(t *testing.T) {
	h, run, ds := vmTree(t, "")
	run.answers["zfs list -H -p -t filesystem,volume -o "+listCols+" -r "+ds] =
		zline(ds, "filesystem", "skep:vm_id", vmA, "encryption", "aes-256-gcm", "keystatus", "unavailable") + vol(ds+"/disk0", vmA, diskA, 11)
	_, err := h.Snapshot(context.Background(), "skeptest", Owner{Kind: OwnerVM, ID: vmA}, snap1)
	wantCode(t, err, models.CodeStorageLocked)

	run.answers["zfs list -H -p -t filesystem,volume -o "+listCols+" -r "+ds] = fsys(ds, vmA, 10) + vol(ds+"/disk0", vmB, diskA, 11)
	if _, err := h.Snapshot(context.Background(), "skeptest", Owner{Kind: OwnerVM, ID: vmA}, snap1); err == nil {
		t.Error("a tree with a dataset of another VM was snapshotted")
	}
	if m := mutations(run.calls); len(m) != 0 {
		t.Errorf("commands ran: %v", m)
	}
}

func TestRollback(t *testing.T) {
	ctx := context.Background()
	o := Owner{Kind: OwnerVM, ID: vmA}

	t.Run("latest", func(t *testing.T) {
		h, run, ds := vmTree(t, threeSnaps("skeptest/skep/vm/"+vmA, ""))
		destroyed, err := h.Rollback(ctx, "skeptest", o, snap3, false)
		if err != nil || len(destroyed) != 0 {
			t.Fatalf("rollback = %v, %v", destroyed, err)
		}
		want := []string{
			"zfs rollback " + ds + "@skep-" + snap3,
			"zfs rollback " + ds + "/disk0@skep-" + snap3,
			"zfs rollback " + ds + "/disk1@skep-" + snap3,
		}
		if m := mutations(run.calls); !reflect.DeepEqual(m, want) {
			t.Errorf("mutations = %v", m)
		}
	})

	t.Run("older without destroy_newer", func(t *testing.T) {
		h, run, _ := vmTree(t, threeSnaps("skeptest/skep/vm/"+vmA, ""))
		_, err := h.Rollback(ctx, "skeptest", o, snap1, false)
		wantCode(t, err, models.CodeSnapshotNotLatest)
		var me *models.Error
		errors.As(err, &me)
		if ids, _ := me.Details["newer_snapshot_ids"].([]string); !reflect.DeepEqual(ids, []string{snap2, snap3}) {
			t.Errorf("newer = %v", me.Details)
		}
		if m := mutations(run.calls); len(m) != 0 {
			t.Errorf("commands ran: %v", m)
		}
	})

	t.Run("older with destroy_newer", func(t *testing.T) {
		h, run, ds := vmTree(t, threeSnaps("skeptest/skep/vm/"+vmA, ""))
		destroyed, err := h.Rollback(ctx, "skeptest", o, snap1, true)
		if err != nil || len(destroyed) != 6 {
			t.Fatalf("rollback = %d destroyed, %v", len(destroyed), err)
		}
		want := []string{
			"zfs rollback -r " + ds + "@skep-" + snap1,
			"zfs rollback -r " + ds + "/disk0@skep-" + snap1,
			"zfs rollback -r " + ds + "/disk1@skep-" + snap1,
		}
		if m := mutations(run.calls); !reflect.DeepEqual(m, want) {
			t.Errorf("mutations = %v", m)
		}
		for _, c := range run.calls {
			if strings.Contains(c, " -R") {
				t.Errorf("forbidden flag: %q", c)
			}
		}
	})

	t.Run("a newer snapshot with clones", func(t *testing.T) {
		h, run, _ := vmTree(t, threeSnaps("skeptest/skep/vm/"+vmA, "skeptest/skep/vm/"+vmB+"/disk0"))
		_, err := h.Rollback(ctx, "skeptest", o, snap1, true)
		wantCode(t, err, models.CodeSnapshotHasClones)
		if m := mutations(run.calls); len(m) != 0 {
			t.Errorf("commands ran: %v", m)
		}
	})

	t.Run("a newer snapshot Skep did not take", func(t *testing.T) {
		ds := "skeptest/skep/vm/" + vmA
		h, run, _ := vmTree(t, threeSnaps(ds, "")+zline(ds+"@manual", "snapshot", "createtxg", "500", "skep:vm_id", vmA))
		_, err := h.Rollback(ctx, "skeptest", o, snap3, true)
		wantCode(t, err, models.CodeSnapshotNotLatest)
		if m := mutations(run.calls); len(m) != 0 {
			t.Errorf("commands ran: %v", m)
		}
	})

	t.Run("a disk added after the snapshot", func(t *testing.T) {
		ds := "skeptest/skep/vm/" + vmA
		// snap1 was taken before disk1 existed.
		h, run, _ := vmTree(t, snapLine(ds, "skep-"+snap1, snap1, vmA, 100, "")+snapLine(ds+"/disk0", "skep-"+snap1, snap1, vmA, 100, ""))
		_, err := h.Rollback(ctx, "skeptest", o, snap1, true)
		wantCode(t, err, models.CodeSnapshotIncomplete)
		if m := mutations(run.calls); len(m) != 0 {
			t.Errorf("commands ran: %v", m)
		}
	})

	t.Run("an unknown snapshot", func(t *testing.T) {
		h, _, _ := vmTree(t, threeSnaps("skeptest/skep/vm/"+vmA, ""))
		if _, err := h.Rollback(ctx, "skeptest", o, "44444444-aaaa-4bbb-8ccc-000000000004", true); !errors.Is(err, models.ErrNotFound) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("a snapshot whose property does not match its name", func(t *testing.T) {
		ds := "skeptest/skep/vm/" + vmA
		bad := snapLine(ds, "skep-"+snap1, snap2, vmA, 100, "") + snapLine(ds+"/disk0", "skep-"+snap1, snap1, vmA, 100, "") +
			snapLine(ds+"/disk1", "skep-"+snap1, snap1, vmA, 100, "")
		h, run, _ := vmTree(t, bad)
		if _, err := h.Rollback(ctx, "skeptest", o, snap1, false); err == nil {
			t.Error("accepted")
		}
		if m := mutations(run.calls); len(m) != 0 {
			t.Errorf("commands ran: %v", m)
		}
	})
}

func TestDestroySnapshot(t *testing.T) {
	ctx := context.Background()
	o := Owner{Kind: OwnerVM, ID: vmA}
	h, run, ds := vmTree(t, threeSnaps("skeptest/skep/vm/"+vmA, ""))
	gone, err := h.DestroySnapshot(ctx, "skeptest", o, snap2)
	if err != nil || len(gone) != 3 {
		t.Fatalf("destroy = %v, %v", gone, err)
	}
	if m := mutations(run.calls); !reflect.DeepEqual(m, []string{"zfs destroy -r " + ds + "@skep-" + snap2}) {
		t.Errorf("mutations = %v", m)
	}

	h, run, _ = vmTree(t, threeSnaps("skeptest/skep/vm/"+vmA, "skeptest/skep/vm/"+vmB+"/disk0"))
	_, err = h.DestroySnapshot(ctx, "skeptest", o, snap2)
	wantCode(t, err, models.CodeSnapshotHasClones)
	if m := mutations(run.calls); len(m) != 0 {
		t.Errorf("commands ran: %v", m)
	}
	if _, err := h.DestroySnapshot(ctx, "skeptest", o, "44444444-aaaa-4bbb-8ccc-000000000004"); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("unknown snapshot: %v", err)
	}
}

func TestCloneVM(t *testing.T) {
	ctx := context.Background()
	h, run, ds := vmTree(t, threeSnaps("skeptest/skep/vm/"+vmA, ""))
	target := "skeptest/skep/vm/" + vmB
	spec := CloneSpec{Pool: "skeptest", SourceVMID: vmA, SnapshotID: snap2, TargetVMID: vmB, Disks: map[int]string{0: diskB, 1: diskA1}}
	if _, err := h.CloneVM(ctx, spec); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"zfs clone -o canmount=off -o skep:vm_id=" + vmB + " " + ds + "@skep-" + snap2 + " " + target,
		"zfs clone -o volmode=dev -o skep:vm_id=" + vmB + " -o skep:disk_id=" + diskB + " " + ds + "/disk0@skep-" + snap2 + " " + target + "/disk0",
		"zfs clone -o volmode=dev -o skep:vm_id=" + vmB + " -o skep:disk_id=" + diskA1 + " " + ds + "/disk1@skep-" + snap2 + " " + target + "/disk1",
	}
	if m := mutations(run.calls); !reflect.DeepEqual(m, want) {
		t.Errorf("mutations:\n%s\nwant:\n%s", strings.Join(m, "\n"), strings.Join(want, "\n"))
	}

	for name, bad := range map[string]CloneSpec{
		"a missing slot":   {Pool: "skeptest", SourceVMID: vmA, SnapshotID: snap2, TargetVMID: vmB, Disks: map[int]string{0: diskB}},
		"an extra slot":    {Pool: "skeptest", SourceVMID: vmA, SnapshotID: snap2, TargetVMID: vmB, Disks: map[int]string{0: diskB, 1: diskA1, 2: diskA}},
		"a bad disk ID":    {Pool: "skeptest", SourceVMID: vmA, SnapshotID: snap2, TargetVMID: vmB, Disks: map[int]string{0: "x", 1: diskA1}},
		"the same VM":      {Pool: "skeptest", SourceVMID: vmA, SnapshotID: snap2, TargetVMID: vmA, Disks: map[int]string{0: diskB, 1: diskA1}},
		"a bad target":     {Pool: "skeptest", SourceVMID: vmA, SnapshotID: snap2, TargetVMID: "../x", Disks: map[int]string{0: diskB, 1: diskA1}},
		"another pool":     {Pool: "zroot", SourceVMID: vmA, SnapshotID: snap2, TargetVMID: vmB, Disks: map[int]string{0: diskB, 1: diskA1}},
		"a bad snapshot":   {Pool: "skeptest", SourceVMID: vmA, SnapshotID: "skep", TargetVMID: vmB, Disks: map[int]string{0: diskB, 1: diskA1}},
		"unknown snapshot": {Pool: "skeptest", SourceVMID: vmA, SnapshotID: "44444444-aaaa-4bbb-8ccc-000000000004", TargetVMID: vmB, Disks: map[int]string{0: diskB, 1: diskA1}},
	} {
		run.calls = nil
		if _, err := h.CloneVM(ctx, bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if m := mutations(run.calls); len(m) != 0 {
			t.Errorf("%s: commands ran: %v", name, m)
		}
	}

	// A target that exists already is left alone.
	run.calls = nil
	run.answers["zfs list -H -p -t filesystem,volume -o "+listCols+" "+target] = fsys(target, vmB, 5)
	if _, err := h.CloneVM(ctx, spec); err == nil {
		t.Error("cloned over an existing dataset")
	}
	if m := mutations(run.calls); len(m) != 0 {
		t.Errorf("commands ran: %v", m)
	}
}

func TestDestroyVMRefusesWhileClonesDepend(t *testing.T) {
	h, run, _ := vmTree(t, threeSnaps("skeptest/skep/vm/"+vmA, "skeptest/skep/vm/"+vmB+"/disk0"))
	_, err := h.DestroyVM(context.Background(), "skeptest", vmA)
	wantCode(t, err, models.CodeSnapshotHasClones)
	if m := mutations(run.calls); len(m) != 0 {
		t.Errorf("commands ran: %v", m)
	}
}

func TestInventoryAndLockedRoot(t *testing.T) {
	ds := "skeptest/skep/vm/" + vmA
	run := &script{answers: map[string]any{
		"zfs list -H -p -t filesystem,volume,snapshot -o " + listCols + " -r skeptest/skep": vol(ds+"/disk0", vmA, diskA, 11) +
			fsys(ds, vmA, 10) + snapLine(ds, "skep-"+snap1, snap1, vmA, 100, "") + fsys("skeptest/skep", "", 2),
	}}
	h := testHost(t, run, true)
	list, err := h.Inventory(context.Background(), "skeptest")
	if err != nil || len(list) != 4 {
		t.Fatalf("inventory = %+v, %v", list, err)
	}
	var names []string
	for _, d := range list {
		names = append(names, d.Name)
	}
	if want := []string{"skeptest/skep", ds, ds + "/disk0", ds + "@skep-" + snap1}; !reflect.DeepEqual(names, want) {
		t.Errorf("order = %v", names)
	}
	if !list[3].IsSnapshot() || list[3].SnapshotID != snap1 || list[2].CompressRatio != "1.00" || list[2].Encryption != "off" {
		t.Errorf("fields = %+v", list[3])
	}
	_, err = h.Inventory(context.Background(), "zroot")
	wantCode(t, err, models.CodePoolNotAllowed)

	// An encrypted root with its key unloaded refuses every create.
	run.answers["zfs list -H -p -t filesystem,volume -o "+listCols+" skeptest/skep"] =
		zline("skeptest/skep", "filesystem", "encryption", "aes-256-gcm", "keystatus", "unavailable")
	run.calls = nil
	_, err = h.CreateVMDataset(context.Background(), "skeptest", vmA)
	wantCode(t, err, models.CodeStorageLocked)
	if m := mutations(run.calls); len(m) != 0 {
		t.Errorf("commands ran: %v", m)
	}
}

func TestVarsSnapshotCopies(t *testing.T) {
	conf := testConf(t)
	f := NewFiles(conf, nil)
	ctx := context.Background()
	vars, _ := conf.VMVarsPath(vmA)
	if err := os.MkdirAll(filepath.Dir(vars), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vars, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.SaveVarsSnapshot(ctx, vmA, snap1); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(filepath.Dir(vars), VarsSnapshotDir, snap1+".fd")
	if b, err := os.ReadFile(copyPath); err != nil || string(b) != "v1" {
		t.Fatalf("copy = %q, %v", b, err)
	}
	if info, _ := os.Stat(copyPath); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode())
	}
	// The guest changes a variable; the rollback brings v1 back.
	_ = os.WriteFile(vars, []byte("v2"), 0o600)
	if err := f.RestoreVarsSnapshot(ctx, vmA, snap1); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(vars); string(b) != "v1" {
		t.Errorf("restored = %q", b)
	}
	// A clone starts from the snapshot's copy.
	if err := f.CloneVars(ctx, vmA, snap1, vmB); err != nil {
		t.Fatal(err)
	}
	cloneVars, _ := conf.VMVarsPath(vmB)
	if b, _ := os.ReadFile(cloneVars); string(b) != "v1" {
		t.Errorf("clone vars = %q", b)
	}
	if err := f.RemoveVarsSnapshot(vmA, snap1); err != nil {
		t.Fatal(err)
	}
	if err := f.RemoveVarsSnapshot(vmA, snap1); err != nil {
		t.Errorf("second remove: %v", err)
	}
	if err := f.RestoreVarsSnapshot(ctx, vmA, snap1); err == nil {
		t.Error("restored from a removed copy")
	}
	// A planted link is not followed.
	_ = os.Symlink("/etc/passwd", filepath.Join(filepath.Dir(vars), VarsSnapshotDir, snap2+".fd"))
	if err := f.RestoreVarsSnapshot(ctx, vmA, snap2); err == nil {
		t.Error("restored through a symbolic link")
	}
	for _, bad := range []string{"", "../x", snap1 + "/.."} {
		if err := f.SaveVarsSnapshot(ctx, vmA, bad); err == nil {
			t.Errorf("snapshot ID %q accepted", bad)
		}
	}
}
