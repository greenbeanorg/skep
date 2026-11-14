package vm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// snapshot takes a snapshot and waits for its task.
func (f *fixture) snapshot(vmID, name string) (models.Snapshot, models.Task) {
	f.t.Helper()
	created, err := f.svc.CreateSnapshot(context.Background(), vmID, models.SnapshotCreate{Name: name}, f.sub)
	if err != nil {
		f.t.Fatalf("snapshot %s: %v", name, err)
	}
	done := f.wait(created.Task.ID)
	snap, err := f.svc.Snapshot(context.Background(), vmID, created.Snapshot.ID)
	if err != nil && done.Status == models.TaskSucceeded {
		f.t.Fatal(err)
	}
	return snap, done.Public()
}

func (f *fixture) varsCopy(vmID, snapID string) string {
	dir, _ := f.conf.VMStateDir(vmID)
	return filepath.Join(dir, hostops.VarsSnapshotDir, snapID+".fd")
}

func writeVars(t *testing.T, f *fixture, vmID, content string) {
	t.Helper()
	path, _ := f.conf.VMVarsPath(vmID)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotLifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	vm := f.create(basicVM("web1"))
	writeVars(t, f, vm.ID, "vars-1")

	s1, task := f.snapshot(vm.ID, "first")
	if task.Status != models.TaskSucceeded {
		t.Fatalf("snapshot: %+v", task)
	}
	if !s1.Provisioned || s1.Consistency != models.SnapshotConsistent || s1.Observed == nil || !s1.Observed.Exists {
		t.Errorf("snapshot = %+v", s1)
	}
	if b, err := os.ReadFile(f.varsCopy(vm.ID, s1.ID)); err != nil || string(b) != "vars-1" {
		t.Errorf("variable store copy = %q, %v", b, err)
	}
	ds := "devpool/skep/vm/" + vm.ID
	calls := strings.Join(f.fake.Calls(), "\n")
	if !strings.Contains(calls, "zfs snapshot -r "+ds+"@skep-"+s1.ID) {
		t.Errorf("calls = %s", calls)
	}

	// Taken while running: crash-consistent.
	if task, err := f.act(vm.ID, models.VMActionStart, nil); err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("start: %v %+v", err, task)
	}
	writeVars(t, f, vm.ID, "vars-2")
	s2, _ := f.snapshot(vm.ID, "second")
	if s2.Consistency != models.SnapshotCrashConsistent {
		t.Errorf("running snapshot consistency = %s", s2.Consistency)
	}
	// Rollback needs the VM stopped.
	_, err := f.svc.RollbackSnapshot(ctx, vm.ID, s2.ID, models.SnapshotRollback{}, f.sub)
	if code(err) != models.CodeVMNotStopped {
		t.Errorf("rollback while running: %v", err)
	}
	if task, err := f.act(vm.ID, models.VMActionForceStop, nil); err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("force stop: %v %+v", err, task)
	}

	// Names are per VM and unique; the label is validated.
	if _, err := f.svc.CreateSnapshot(ctx, vm.ID, models.SnapshotCreate{Name: "first"}, f.sub); code(err) != models.CodeSnapshotNameTaken {
		t.Errorf("duplicate name: %v", err)
	}
	for _, bad := range []string{"", "-x", "a/b", "a@b", strings.Repeat("a", 64)} {
		if _, err := f.svc.CreateSnapshot(ctx, vm.ID, models.SnapshotCreate{Name: bad}, f.sub); code(err) != models.CodeInvalidRequest {
			t.Errorf("name %q: %v", bad, err)
		}
	}

	// Rollback to the older one needs destroy_newer.
	_, err = f.svc.RollbackSnapshot(ctx, vm.ID, s1.ID, models.SnapshotRollback{}, f.sub)
	if code(err) != models.CodeSnapshotNotLatest {
		t.Fatalf("rollback without destroy_newer: %v", err)
	}
	writeVars(t, f, vm.ID, "vars-3")
	rb, err := f.svc.RollbackSnapshot(ctx, vm.ID, s1.ID, models.SnapshotRollback{DestroyNewer: true}, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	done := f.wait(rb.ID)
	if done.Status != models.TaskSucceeded {
		t.Fatalf("rollback: %s %s", done.ErrorCode, done.ErrorMessage)
	}
	var result RollbackResult
	_ = json.Unmarshal(done.Result, &result)
	if len(result.Destroyed) != 1 || result.Destroyed[0].ID != s2.ID || result.Destroyed[0].Name != "second" {
		t.Errorf("rollback result = %+v", result)
	}
	vars, _ := f.conf.VMVarsPath(vm.ID)
	if b, _ := os.ReadFile(vars); string(b) != "vars-1" {
		t.Errorf("variable store after rollback = %q", b)
	}
	if _, err := os.Stat(f.varsCopy(vm.ID, s2.ID)); !os.IsNotExist(err) {
		t.Errorf("the destroyed snapshot's copy is still there: %v", err)
	}
	list, _ := f.svc.Snapshots(ctx, vm.ID)
	if len(list.Items) != 1 || list.Items[0].ID != s1.ID {
		t.Errorf("snapshots after rollback = %+v", list.Items)
	}
	// The audit entry names what was destroyed.
	var details string
	if err := f.db.QueryRow(`SELECT details FROM audit_log WHERE action = ? AND phase = 'finished' AND task_id = ?`,
		models.TaskVMSnapshotRollback, done.ID).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, s2.ID) || !strings.Contains(details, "snapshots_destroyed") {
		t.Errorf("audit details = %s", details)
	}

	// Delete.
	del, err := f.svc.DeleteSnapshot(ctx, vm.ID, s1.ID, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	if done := f.wait(del.ID); done.Status != models.TaskSucceeded {
		t.Fatalf("delete: %s %s", done.ErrorCode, done.ErrorMessage)
	}
	if _, err := os.Stat(f.varsCopy(vm.ID, s1.ID)); !os.IsNotExist(err) {
		t.Error("the deleted snapshot's copy is still there")
	}
	if n := f.count(`SELECT COUNT(*) FROM snapshots`); n != 0 {
		t.Errorf("%d snapshot rows left", n)
	}
	for _, name := range f.fake.Datasets() {
		if strings.Contains(name, "@") {
			t.Errorf("snapshot left in ZFS: %s", name)
		}
	}
}

func TestSnapshotLimitAndReplay(t *testing.T) {
	f := newFixture(t)
	f.svc.maxSnapshots = 2
	ctx := context.Background()
	vm := f.create(basicVM("web1"))
	f.snapshot(vm.ID, "a")
	f.snapshot(vm.ID, "b")
	if _, err := f.svc.CreateSnapshot(ctx, vm.ID, models.SnapshotCreate{Name: "c"}, f.sub); code(err) != models.CodeSnapshotLimit {
		t.Errorf("third snapshot: %v", err)
	}
	// A replay with the same key returns the same snapshot and task.
	f.svc.maxSnapshots = 10
	sub := f.sub
	sub.IdempotencyKey = "key-1"
	first, err := f.svc.CreateSnapshot(ctx, vm.ID, models.SnapshotCreate{Name: "c"}, sub)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.svc.CreateSnapshot(ctx, vm.ID, models.SnapshotCreate{Name: "c"}, sub)
	if err != nil || again.Snapshot.ID != first.Snapshot.ID || again.Task.ID != first.Task.ID {
		t.Errorf("replay = %+v, %v", again, err)
	}
	if _, err := f.svc.CreateSnapshot(ctx, vm.ID, models.SnapshotCreate{Name: "d"}, sub); code(err) != models.CodeIdempotencyConflict {
		t.Errorf("key reused for another request: %v", err)
	}
}

func TestSnapshotRefusals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// A VM with only install media has nothing to snapshot.
	req := basicVM("media-only")
	req.Disks = req.Disks[1:]
	vm := f.create(req)
	if _, err := f.svc.CreateSnapshot(ctx, vm.ID, models.SnapshotCreate{Name: "a"}, f.sub); code(err) != models.CodeStorageFeatureUnsupported {
		t.Errorf("no disks: %v", err)
	}
	// A locked dataset fails the task with storage_locked.
	vm2 := f.create(basicVM("web2"))
	f.fake.Lock("devpool/skep/vm/"+vm2.ID, true)
	_, task := f.snapshot(vm2.ID, "a")
	if task.Status != models.TaskFailed || task.Error == nil || task.Error.Code != models.CodeStorageLocked {
		t.Errorf("locked: %+v", task)
	}
	// The failed snapshot left neither a row nor a copy.
	if n := f.count(`SELECT COUNT(*) FROM snapshots WHERE resource_id = ?`, vm2.ID); n != 0 {
		t.Errorf("%d rows after a failed snapshot", n)
	}
	dir, _ := f.conf.VMStateDir(vm2.ID)
	if entries, _ := os.ReadDir(filepath.Join(dir, hostops.VarsSnapshotDir)); len(entries) != 0 {
		t.Errorf("copies after a failed snapshot: %v", entries)
	}
	// Every refusal is audited.
	if n := f.count(`SELECT COUNT(*) FROM audit_log WHERE action = ? AND outcome = 'denied'`, models.TaskVMSnapshotCreate); n < 1 {
		t.Error("refusals were not audited")
	}
}

func TestCloneLifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	src := f.create(basicVM("web1"))
	writeVars(t, f, src.ID, "src-vars")
	s1, _ := f.snapshot(src.ID, "golden")

	created, err := f.svc.CloneVM(ctx, src.ID, models.VMClone{Name: "web1-clone", SnapshotID: s1.ID}, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	if done := f.wait(created.Task.ID); done.Status != models.TaskSucceeded {
		t.Fatalf("clone: %s %s", done.ErrorCode, done.ErrorMessage)
	}
	clone, _ := f.svc.VM(ctx, created.VM.ID)
	if !clone.Provisioned || clone.ClonedFromSnapshotID != s1.ID || clone.Name != "web1-clone" || clone.ID == src.ID {
		t.Fatalf("clone = %+v", clone)
	}
	if len(clone.Disks) != 1 || !clone.Disks[0].Thin || clone.Disks[0].SizeBytes != src.Disks[0].SizeBytes {
		t.Errorf("clone disks = %+v (install media is not copied)", clone.Disks)
	}
	if clone.NICs[0].MACAddress == src.NICs[0].MACAddress || clone.NICs[0].Bridge != src.NICs[0].Bridge {
		t.Errorf("clone NIC = %+v, source %+v", clone.NICs[0], src.NICs[0])
	}
	cloneVars, _ := f.conf.VMVarsPath(clone.ID)
	if b, _ := os.ReadFile(cloneVars); string(b) != "src-vars" {
		t.Errorf("clone variable store = %q", b)
	}
	snap, _ := f.svc.Snapshot(ctx, src.ID, s1.ID)
	if len(snap.Clones) != 1 || snap.Clones[0].ID != clone.ID {
		t.Errorf("snapshot clones = %+v", snap.Clones)
	}

	// The origin snapshot and the source are protected.
	if _, err := f.svc.DeleteSnapshot(ctx, src.ID, s1.ID, f.sub); code(err) != models.CodeSnapshotHasClones {
		t.Errorf("delete the origin: %v", err)
	}
	if _, err := f.svc.DeleteVM(ctx, src.ID, f.sub); code(err) != models.CodeSnapshotHasClones {
		t.Errorf("delete the source: %v", err)
	}
	// A clone boots.
	if task, err := f.act(clone.ID, models.VMActionStart, nil); err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("start the clone: %v %+v", err, task)
	}
	if task, err := f.act(clone.ID, models.VMActionForceStop, nil); err != nil || task.Status != models.TaskSucceeded {
		t.Fatalf("stop the clone: %v %+v", err, task)
	}

	// A clone from the running state takes a snapshot named after it,
	// and leaves it.
	created2, err := f.svc.CloneVM(ctx, src.ID, models.VMClone{Name: "web1-live"}, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	if done := f.wait(created2.Task.ID); done.Status != models.TaskSucceeded {
		t.Fatalf("clone from now: %s %s", done.ErrorCode, done.ErrorMessage)
	}
	list, _ := f.svc.Snapshots(ctx, src.ID)
	if len(list.Items) != 2 || list.Items[1].Name != "web1-live" || !list.Items[1].Provisioned {
		t.Errorf("snapshots after clone-from-now = %+v", list.Items)
	}

	// Delete the clones, then the source can go.
	for _, id := range []string{clone.ID, created2.VM.ID} {
		del, err := f.svc.DeleteVM(ctx, id, f.sub)
		if err != nil {
			t.Fatal(err)
		}
		if done := f.wait(del.ID); done.Status != models.TaskSucceeded {
			t.Fatalf("delete clone: %s %s", done.ErrorCode, done.ErrorMessage)
		}
	}
	del, err := f.svc.DeleteVM(ctx, src.ID, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	if done := f.wait(del.ID); done.Status != models.TaskSucceeded {
		t.Fatalf("delete source: %s %s", done.ErrorCode, done.ErrorMessage)
	}
	if got := f.fake.Datasets(); len(got) != 0 {
		t.Errorf("datasets left: %v", got)
	}

	// Refusals.
	other := f.create(basicVM("web2"))
	for name, req := range map[string]models.VMClone{
		"bad name":         {Name: "a b"},
		"unknown snapshot": {Name: "x", SnapshotID: "11111111-aaaa-4bbb-8ccc-000000000009"},
		"taken name":       {Name: "web2"},
	} {
		if _, err := f.svc.CloneVM(ctx, other.ID, req, f.sub); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRollbackPastAddedDiskIsRefused(t *testing.T) {
	f := newFixture(t)
	vm := f.create(basicVM("web1"))
	s1, _ := f.snapshot(vm.ID, "before")
	// A disk added behind Skep's back, as step 3's disk add will.
	if _, err := f.fake.Ops().ZFS.CreateVolume(context.Background(), hostops.VolumeSpec{Pool: "devpool", VMID: vm.ID,
		DiskID: "7e3c1a90-1111-4222-8333-444455556699", Slot: 5, SizeBytes: 1 << 30, BlockSize: 16384, Thin: true}); err != nil {
		t.Fatal(err)
	}
	rb, err := f.svc.RollbackSnapshot(context.Background(), vm.ID, s1.ID, models.SnapshotRollback{}, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	if done := f.wait(rb.ID); done.Status != models.TaskFailed || done.ErrorCode != models.CodeSnapshotIncomplete {
		t.Errorf("rollback = %s %s", done.Status, done.ErrorCode)
	}
}
