package vm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/hostops/hostfake"
	"github.com/greenbeanorg/skep/internal/hypervisor/bhyve"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/migrations"
	"github.com/greenbeanorg/skep/pkg/models"
)

const (
	gib      = int64(1) << 30
	testISO  = "FreeBSD-15.1-RELEASE-amd64-disc1.iso"
	poolSize = 20 * gib
)

// fixture is a VM service on a scratch agent.db, a real task engine, and
// the fake host.
type fixture struct {
	t      *testing.T
	db     *sql.DB
	fake   *hostfake.Fake
	engine *tasks.Engine
	svc    *Service
	conf   hostops.Confinement
	sub    Submission
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := database.Open(ctx, filepath.Join(dir, "agent", "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := database.Migrate(ctx, db, migrations.Agent()); err != nil {
		t.Fatal(err)
	}
	nodeID, err := database.EnsureLocalNode(ctx, db, "testnode")
	if err != nil {
		t.Fatal(err)
	}
	conf := hostops.Confinement{
		Pools: []string{"devpool"}, DatasetRoot: "skep", MediaDir: filepath.Join(dir, "media"),
		Bridges: []string{"bridge0"}, ManagementInterfaces: []string{"vtnet0"},
		FirmwareDir: filepath.Join(dir, "firmware"),
		RunDir:      filepath.Join(dir, "run", "vm"), StateDir: filepath.Join(dir, "agent", "vm"),
	}
	if err := os.MkdirAll(conf.MediaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conf.MediaDir, testISO), []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := hostfake.New(conf, hostfake.Options{PoolBytes: poolSize, HostCPUs: 4, HostMemoryBytes: 6 * gib, Logger: quiet,
		Runner: func(ctx context.Context, ops hostops.Ops, vmID string, pid int, signals <-chan hostops.RunnerSignal) error {
			return bhyve.RunVM(ctx, ops, vmID, pid, signals, quiet)
		}})
	t.Cleanup(fake.Close)
	engine, err := tasks.New(tasks.Options{
		Store: database.NewTaskStore(db), Audit: database.NewAuditLog(db), NodeID: nodeID, Logger: quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	host := fake.Ops()
	svc, err := New(Options{
		NodeID: nodeID, Store: database.NewVMStore(db), Snapshots: database.NewSnapshotStore(db), Engine: engine, Host: host, Logger: quiet,
		Hypervisor: bhyve.New(bhyve.Options{Host: host, HostMemoryReserveBytes: gib, Logger: quiet}),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, def := range svc.Definitions() {
		if err := engine.Register(def); err != nil {
			t.Fatal(err)
		}
	}
	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = engine.Shutdown(sctx)
	})
	return &fixture{
		t: t, db: db, fake: fake, engine: engine, svc: svc, conf: conf,
		sub: Submission{Actor: models.TaskActor{ID: "u-1", Name: "andy"}, PeerUID: 1001, RequestID: "req-1", SourceIP: "192.0.2.7"},
	}
}

// wait returns a task once it is finished.
func (f *fixture) wait(id string) tasks.Task {
	f.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		task, err := f.engine.Get(context.Background(), id)
		if err != nil {
			f.t.Fatal(err)
		}
		if task.Status.Terminal() {
			return task
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatalf("task %s did not finish", id)
	return tasks.Task{}
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func basicVM(name string) models.VMCreate {
	return models.VMCreate{
		Name: name, CPUCount: 2, Memory: models.ByteSize(gib),
		Disks: []models.VMDiskSpec{
			{Pool: "devpool", Size: models.ByteSize(8 * gib), Thin: true},
			{Media: testISO},
		},
		NICs: []models.VMNICSpec{{Bridge: "bridge0"}},
	}
}

// create creates a VM and waits for its task to succeed.
func (f *fixture) create(req models.VMCreate) models.VM {
	f.t.Helper()
	created, err := f.svc.CreateVM(context.Background(), req, f.sub)
	if err != nil {
		f.t.Fatal(err)
	}
	if task := f.wait(created.Task.ID); task.Status != models.TaskSucceeded {
		f.t.Fatalf("vm.create: %s %s %s", task.Status, task.ErrorCode, task.ErrorMessage)
	}
	vm, err := f.svc.VM(context.Background(), created.VM.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return vm
}

func code(err error) string {
	var me *models.Error
	if errors.As(err, &me) {
		return me.Code
	}
	if err != nil {
		return "error: " + err.Error()
	}
	return ""
}

func TestCreateProvisionsAVM(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	created, err := f.svc.CreateVM(ctx, basicVM("web-1"), f.sub)
	if err != nil {
		t.Fatal(err)
	}
	// The VM exists at once, with its name reserved. It is not
	// provisioned unless a worker already ran the task between the submit
	// and the read that built the answer.
	vm := created.VM
	if vm.Provisioned {
		if task, err := f.engine.Get(ctx, created.Task.ID); err != nil || task.Status != models.TaskSucceeded {
			t.Fatalf("the VM was provisioned before its task succeeded: %+v, %v", task, err)
		}
	}
	if vm.ID == "" || vm.Name != "web-1" || vm.Engine != "bhyve" || vm.Firmware != "uefi" ||
		vm.DesiredState != models.VMStopped || len(vm.Disks) != 2 || len(vm.NICs) != 1 {
		t.Fatalf("created VM = %+v", vm)
	}
	if created.Task.Type != models.TaskVMCreate || created.Task.ResourceID != vm.ID || created.Task.Actor.Name != "andy" {
		t.Errorf("task = %+v", created.Task)
	}
	disk, cd, nic := vm.Disks[0], vm.Disks[1], vm.NICs[0]
	if disk.Bus != models.BusVirtioBlk || disk.SizeBytes != 8*gib || !disk.Thin || disk.VolBlockSize != 16384 ||
		disk.Dataset != "devpool/skep/vm/"+vm.ID+"/disk0" || disk.Slot != 0 {
		t.Errorf("disk = %+v", disk)
	}
	if cd.Bus != models.BusAHCICD || cd.Media != testISO || !cd.ReadOnly || cd.Slot != 1 || cd.Dataset != "" {
		t.Errorf("media = %+v", cd)
	}
	if !strings.HasPrefix(nic.MACAddress, "02:") || len(nic.MACAddress) != 17 || nic.Model != models.NICVirtioNet {
		t.Errorf("nic = %+v", nic)
	}

	task := f.wait(created.Task.ID)
	if task.Status != models.TaskSucceeded {
		t.Fatalf("task = %s %s", task.Status, task.ErrorMessage)
	}
	var result CreateResult
	if err := json.Unmarshal(task.Result, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Datasets) != 2 || result.Datasets[1].Name != disk.Dataset || result.Datasets[1].SizeBytes != 8*gib {
		t.Errorf("result = %+v", result)
	}
	vm, _ = f.svc.VM(ctx, vm.ID)
	if !vm.Provisioned || vm.Observed.State != models.VMStopped || vm.Drift {
		t.Errorf("after create: provisioned=%v observed=%+v drift=%v", vm.Provisioned, vm.Observed, vm.Drift)
	}
	want := []string{"devpool/skep/vm/" + vm.ID, disk.Dataset}
	if got := f.fake.Datasets(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("datasets = %v", got)
	}
	vars, _ := f.conf.VMVarsPath(vm.ID)
	if _, err := os.Stat(vars); err != nil {
		t.Errorf("the variable store was not prepared: %v", err)
	}

	// Both phases are in the operation audit log, with the actor, the
	// verified peer, and the datasets.
	if n := f.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'vm.create' AND resource_id = ? AND phase = 'requested'
		AND outcome = 'success' AND actor_name = 'andy' AND peer_uid = 1001 AND request_id = 'req-1'`, vm.ID); n != 1 {
		t.Errorf("%d requested entries", n)
	}
	var details string
	if err := f.db.QueryRow(`SELECT details FROM audit_log WHERE action = 'vm.create' AND phase = 'finished' AND task_id = ?`,
		task.ID).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, disk.Dataset) || !strings.Contains(details, "datasets_created") {
		t.Errorf("finished entry details = %s", details)
	}
	// The volume row carries what was asked for and what was observed.
	if n := f.count(`SELECT COUNT(*) FROM storage_volumes WHERE name = ? AND desired_thin = 1 AND desired_volblocksize = 16384
		AND desired_size_bytes = ? AND observed_exists = 1 AND observed_volsize_bytes = ?`,
		"skep/vm/"+vm.ID+"/disk0", 8*gib, 8*gib); n != 1 {
		t.Error("the volume row is not what was created")
	}
}

func TestCreateIsRefusedBeforeAnythingIsMade(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.create(basicVM("taken"))
	rowsBefore := f.count(`SELECT COUNT(*) FROM virtual_machines`)
	tasksBefore := f.count(`SELECT COUNT(*) FROM tasks`)
	datasetsBefore := len(f.fake.Datasets())

	with := func(change func(*models.VMCreate)) models.VMCreate {
		req := basicVM("new-vm")
		change(&req)
		return req
	}
	for name, tt := range map[string]struct {
		req     models.VMCreate
		code    string
		outcome string
	}{
		"bad name":      {with(func(r *models.VMCreate) { r.Name = "web 1; reboot" }), models.CodeInvalidRequest, "failure"},
		"no cpu":        {with(func(r *models.VMCreate) { r.CPUCount = 0 }), models.CodeInvalidRequest, "failure"},
		"too many cpus": {with(func(r *models.VMCreate) { r.CPUCount = 5 }), models.CodeInvalidRequest, "failure"},
		"small memory":  {with(func(r *models.VMCreate) { r.Memory = 64 << 20 }), models.CodeInvalidRequest, "failure"},
		"odd memory":    {with(func(r *models.VMCreate) { r.Memory = models.ByteSize(gib + 1) }), models.CodeInvalidRequest, "failure"},
		"memory > host": {with(func(r *models.VMCreate) { r.Memory = models.ByteSize(6 * gib) }), models.CodeInvalidRequest, "failure"},
		"bad topology": {with(func(r *models.VMCreate) {
			r.Config = &models.VMConfig{CPU: &models.VMCPUTopology{Sockets: 1, Cores: 3, Threads: 1}}
		}), models.CodeInvalidRequest, "failure"},
		"unknown engine": {with(func(r *models.VMCreate) { r.Engine = "kvm" }), models.CodeInvalidRequest, "failure"},
		"qemu":           {with(func(r *models.VMCreate) { r.Engine = "qemu" }), models.CodeEngineUnsupported, "denied"},
		"bios":           {with(func(r *models.VMCreate) { r.Firmware = "bios" }), models.CodeFirmwareUnsupported, "denied"},
		"vnc": {with(func(r *models.VMCreate) {
			r.Config = &models.VMConfig{Console: models.VMConsoleConfig{VNC: models.VMVNCConfig{Enabled: true}}}
		}), models.CodeNotImplemented, "denied"},
		"pool":           {with(func(r *models.VMCreate) { r.Disks[0].Pool = "zroot" }), models.CodePoolNotAllowed, "denied"},
		"pool as a path": {with(func(r *models.VMCreate) { r.Disks[0].Pool = "devpool/../zroot" }), models.CodeInvalidRequest, "failure"},
		"bridge":         {with(func(r *models.VMCreate) { r.NICs[0].Bridge = "bridge1" }), models.CodeBridgeNotAllowed, "denied"},
		"management nic": {with(func(r *models.VMCreate) { r.NICs[0].Bridge = "vtnet0" }), models.CodeBridgeNotAllowed, "denied"},
		"full too big": {with(func(r *models.VMCreate) {
			r.Disks[0] = models.VMDiskSpec{Pool: "devpool", Size: models.ByteSize(poolSize + gib)}
		}), models.CodeInsufficientSpace, "denied"},
		"thin > pool":     {with(func(r *models.VMCreate) { r.Disks[0].Size = models.ByteSize(poolSize + gib) }), models.CodeInsufficientSpace, "denied"},
		"small disk":      {with(func(r *models.VMCreate) { r.Disks[0].Size = 1 << 20 }), models.CodeInvalidRequest, "failure"},
		"bad block size":  {with(func(r *models.VMCreate) { r.Disks[0].VolBlockSize = 3000 }), models.CodeInvalidRequest, "failure"},
		"bad bus":         {with(func(r *models.VMCreate) { r.Disks[0].Bus = "ide" }), models.CodeInvalidRequest, "failure"},
		"media missing":   {with(func(r *models.VMCreate) { r.Disks[1].Media = "missing.iso" }), models.CodeMediaNotFound, "denied"},
		"media as a path": {with(func(r *models.VMCreate) { r.Disks[1].Media = "../../etc/passwd" }), models.CodeInvalidRequest, "failure"},
		"media with size": {with(func(r *models.VMCreate) { r.Disks[1].Size = models.ByteSize(gib) }), models.CodeInvalidRequest, "failure"},
		"multicast mac":   {with(func(r *models.VMCreate) { r.NICs[0].MACAddress = "01:00:5e:00:00:01" }), models.CodeInvalidRequest, "failure"},
		"bad mac":         {with(func(r *models.VMCreate) { r.NICs[0].MACAddress = "not-a-mac" }), models.CodeInvalidRequest, "failure"},
		"too many nics":   {with(func(r *models.VMCreate) { r.NICs = make([]models.VMNICSpec, 5) }), models.CodeInvalidRequest, "failure"},
		"too many disks":  {with(func(r *models.VMCreate) { r.Disks = make([]models.VMDiskSpec, 9) }), models.CodeInvalidRequest, "failure"},
		"name taken":      {with(func(r *models.VMCreate) { r.Name = "taken" }), models.CodeVMNameTaken, "denied"},
	} {
		before := f.count(`SELECT COUNT(*) FROM audit_log`)
		_, err := f.svc.CreateVM(ctx, tt.req, f.sub)
		if code(err) != tt.code {
			t.Errorf("%s: %v, want %s", name, err, tt.code)
			continue
		}
		// One "requested" entry with the outcome and the code, and no task.
		if n := f.count(`SELECT COUNT(*) FROM audit_log WHERE id > ? AND action = 'vm.create' AND phase = 'requested'
			AND outcome = ? AND task_id IS NULL AND actor_name = 'andy' AND details LIKE ?`,
			before, tt.outcome, `%"error_code":"`+tt.code+`"%`); n != 1 {
			t.Errorf("%s: the refusal is not audited as %s", name, tt.outcome)
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM virtual_machines`); n != rowsBefore {
		t.Errorf("%d VM rows, want %d", n, rowsBefore)
	}
	if n := f.count(`SELECT COUNT(*) FROM tasks`); n != tasksBefore {
		t.Errorf("%d tasks, want %d", n, tasksBefore)
	}
	if n := len(f.fake.Datasets()); n != datasetsBefore {
		t.Errorf("%d datasets, want %d", n, datasetsBefore)
	}
	if n := f.count(`SELECT COUNT(*) FROM storage_volumes`); n != 1 {
		t.Errorf("%d volume rows, want 1", n)
	}

	// A MAC address already in use is refused by the companion.
	taken, _ := f.svc.VMs(ctx, models.VMListOptions{})
	dup := basicVM("dup-mac")
	dup.NICs[0].MACAddress = taken.Items[0].NICs[0].MACAddress
	if _, err := f.svc.CreateVM(ctx, dup, f.sub); code(err) != models.CodeInvalidRequest {
		t.Errorf("duplicate MAC: %v", err)
	}
	// Without ZFS nothing can be created, and nothing is.
	f.fake.Set(func(fk *hostfake.Fake) { fk.ZFSLoaded = false })
	if _, err := f.svc.CreateVM(ctx, basicVM("no-zfs"), f.sub); code(err) != models.CodeStorageUnavailable {
		t.Errorf("without zfs: %v", err)
	}
}

func TestCreateWarnsAboutThinDisksAndReservesFullOnes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A full disk takes its size from the pool.
	full := basicVM("full")
	full.Disks = []models.VMDiskSpec{{Pool: "devpool", Size: models.ByteSize(15 * gib)}}
	f.create(full)
	pools, err := f.svc.StoragePools(ctx)
	if err != nil || len(pools.Items) != 1 || pools.Items[0].AvailableBytes != 5*gib || pools.Items[0].Health != "ONLINE" {
		t.Fatalf("pools = %+v, %v", pools, err)
	}
	// A second full disk that does not fit is refused; a thin one of the
	// same size is created with a warning.
	more := basicVM("more")
	more.Disks = []models.VMDiskSpec{{Pool: "devpool", Size: models.ByteSize(8 * gib)}}
	if _, err := f.svc.CreateVM(ctx, more, f.sub); code(err) != models.CodeInsufficientSpace {
		t.Fatalf("overcommitted full disk: %v", err)
	}
	more.Disks[0].Thin = true
	created, err := f.svc.CreateVM(ctx, more, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	task := f.wait(created.Task.ID)
	var result CreateResult
	_ = json.Unmarshal(task.Result, &result)
	if task.Status != models.TaskSucceeded || len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "thin") {
		t.Errorf("task = %s, warnings = %v", task.Status, result.Warnings)
	}
}

func TestCreateReplayAndFailure(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.sub.IdempotencyKey = "create-web-1"

	first, err := f.svc.CreateVM(ctx, basicVM("web-1"), f.sub)
	if err != nil {
		t.Fatal(err)
	}
	f.wait(first.Task.ID)
	replay, err := f.svc.CreateVM(ctx, basicVM("web-1"), f.sub)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Task.ID != first.Task.ID || replay.VM.ID != first.VM.ID || !replay.VM.Provisioned {
		t.Errorf("replay = task %s vm %s, first = task %s vm %s", replay.Task.ID, replay.VM.ID, first.Task.ID, first.VM.ID)
	}
	if n := f.count(`SELECT COUNT(*) FROM virtual_machines`); n != 1 {
		t.Errorf("%d VMs after a replay", n)
	}
	// The same key for something else is a conflict, for anyone.
	if _, err := f.svc.CreateVM(ctx, basicVM("web-2"), f.sub); code(err) != models.CodeIdempotencyConflict {
		t.Errorf("same key, other request: %v", err)
	}
	other := f.sub
	other.Actor = models.TaskActor{ID: "u-2", Name: "dana"}
	if _, err := f.svc.CreateVM(ctx, basicVM("web-1"), other); code(err) != models.CodeIdempotencyConflict {
		t.Errorf("same key, other actor: %v", err)
	}

	// A create that fails on the host leaves nothing behind.
	f.sub.IdempotencyKey = ""
	f.fake.Set(func(fk *hostfake.Fake) { fk.FailCreateVolume = errors.New("zfs: I/O error") })
	failed, err := f.svc.CreateVM(ctx, basicVM("doomed"), f.sub)
	if err != nil {
		t.Fatal(err)
	}
	task := f.wait(failed.Task.ID)
	if task.Status != models.TaskFailed || task.ErrorCode != tasks.CodeFailed {
		t.Errorf("task = %s %s", task.Status, task.ErrorCode)
	}
	if _, err := f.svc.VM(ctx, failed.VM.ID); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("the failed VM still exists: %v", err)
	}
	for _, d := range f.fake.Datasets() {
		if strings.Contains(d, failed.VM.ID) {
			t.Errorf("dataset %s survived the failed create", d)
		}
	}
	state, _ := f.conf.VMStateDir(failed.VM.ID)
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Error("the state directory survived the failed create")
	}
	// The name is free again.
	f.fake.Set(func(fk *hostfake.Fake) { fk.FailCreateVolume = nil })
	f.create(basicVM("doomed"))
}

func TestUpdate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	vm := f.create(basicVM("web-1"))
	if err := os.WriteFile(filepath.Join(f.conf.MediaDir, "other.iso"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	update := func(upd models.VMUpdate) tasks.Task {
		t.Helper()
		task, err := f.svc.UpdateVM(ctx, vm.ID, upd, f.sub)
		if err != nil {
			t.Fatal(err)
		}
		return f.wait(task.ID)
	}
	cpus, mem, desc := 4, models.ByteSize(2*gib), "the web server"
	if task := update(models.VMUpdate{CPUCount: &cpus, Memory: &mem, Description: &desc}); task.Status != models.TaskSucceeded {
		t.Fatalf("update: %s %s", task.Status, task.ErrorMessage)
	}
	got, _ := f.svc.VM(ctx, vm.ID)
	if got.CPUCount != 4 || got.MemoryBytes != 2*gib || got.Description != desc || len(got.Disks) != 2 {
		t.Errorf("after update = %+v", got)
	}

	// Detach the install media, then attach two.
	none := []string{}
	update(models.VMUpdate{Media: &none})
	got, _ = f.svc.VM(ctx, vm.ID)
	if len(got.Disks) != 1 || got.Disks[0].Bus != models.BusVirtioBlk {
		t.Errorf("after detach = %+v", got.Disks)
	}
	two := []string{"other.iso", testISO}
	update(models.VMUpdate{Media: &two})
	got, _ = f.svc.VM(ctx, vm.ID)
	if len(got.Disks) != 3 || got.Disks[1].Media != "other.iso" || got.Disks[1].Slot != 1 ||
		got.Disks[2].Media != testISO || got.Disks[2].Slot != 2 || got.Disks[0].Dataset != vm.Disks[0].Dataset {
		t.Errorf("after attach = %+v", got.Disks)
	}

	for name, tt := range map[string]struct {
		upd  models.VMUpdate
		code string
	}{
		"missing media": {models.VMUpdate{Media: &[]string{"gone.iso"}}, models.CodeMediaNotFound},
		"media path":    {models.VMUpdate{Media: &[]string{"../x.iso"}}, models.CodeInvalidRequest},
		"media twice":   {models.VMUpdate{Media: &[]string{testISO, testISO}}, models.CodeInvalidRequest},
		"no cpus":       {models.VMUpdate{CPUCount: new(int)}, models.CodeInvalidRequest},
		"vnc":           {models.VMUpdate{Config: &models.VMConfig{Console: models.VMConsoleConfig{VNC: models.VMVNCConfig{Enabled: true}}}}, models.CodeNotImplemented},
	} {
		if _, err := f.svc.UpdateVM(ctx, vm.ID, tt.upd, f.sub); code(err) != tt.code {
			t.Errorf("%s: %v, want %s", name, err, tt.code)
		}
	}
	if _, err := f.svc.UpdateVM(ctx, "0b0b6f4e-5d0c-4a52-9a43-0d2f8a1f3c11", models.VMUpdate{}, f.sub); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("unknown VM: %v", err)
	}

	// A VM with something running on the host is not updated.
	f.fake.SetInstance(vm.ID, true)
	if _, err := f.svc.UpdateVM(ctx, vm.ID, models.VMUpdate{CPUCount: &cpus}, f.sub); code(err) != models.CodeVMNotStopped {
		t.Errorf("update of an active VM: %v", err)
	}
}

func TestDelete(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	vm := f.create(basicVM("web-1"))
	keep := f.create(basicVM("keep"))

	// Not while anything of it exists on the host.
	f.fake.SetInstance(vm.ID, true)
	if _, err := f.svc.DeleteVM(ctx, vm.ID, f.sub); code(err) != models.CodeVMNotStopped {
		t.Fatalf("delete of an active VM: %v", err)
	}
	if n := f.count(`SELECT COUNT(*) FROM audit_log WHERE action = 'vm.delete' AND outcome = 'denied' AND resource_id = ?`, vm.ID); n != 1 {
		t.Error("the refused delete is not audited")
	}
	f.fake.SetInstance(vm.ID, false)

	task, err := f.svc.DeleteVM(ctx, vm.ID, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	done := f.wait(task.ID)
	if done.Status != models.TaskSucceeded {
		t.Fatalf("delete: %s %s", done.Status, done.ErrorMessage)
	}
	if _, err := f.svc.VM(ctx, vm.ID); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("the VM still exists: %v", err)
	}
	for _, d := range f.fake.Datasets() {
		if strings.Contains(d, vm.ID) {
			t.Errorf("dataset %s survived", d)
		}
	}
	if len(f.fake.Datasets()) != 2 {
		t.Errorf("the other VM's datasets were touched: %v", f.fake.Datasets())
	}
	state, _ := f.conf.VMStateDir(vm.ID)
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Error("the state directory survived")
	}
	if _, err := os.Stat(filepath.Join(f.conf.MediaDir, testISO)); err != nil {
		t.Error("the media file was touched")
	}
	if n := f.count(`SELECT (SELECT COUNT(*) FROM vm_disks WHERE vm_id = ?) + (SELECT COUNT(*) FROM vm_networks WHERE vm_id = ?)
		+ (SELECT COUNT(*) FROM storage_volumes WHERE name LIKE ?)`, vm.ID, vm.ID, "%"+vm.ID+"%"); n != 0 {
		t.Errorf("%d rows of the VM are left", n)
	}
	// The audit entry lists what was destroyed, with sizes.
	var details string
	if err := f.db.QueryRow(`SELECT details FROM audit_log WHERE action = 'vm.delete' AND phase = 'finished' AND task_id = ?`,
		task.ID).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, "datasets_destroyed") || !strings.Contains(details, vm.Disks[0].Dataset) ||
		!strings.Contains(details, `"size_bytes":8589934592`) {
		t.Errorf("finished entry details = %s", details)
	}
	if got, err := f.svc.VM(ctx, keep.ID); err != nil || !got.Provisioned {
		t.Errorf("the other VM: %+v, %v", got, err)
	}
	// Gone is gone.
	if _, err := f.svc.DeleteVM(ctx, vm.ID, f.sub); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
}

func TestListAndMedia(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, name := range []string{"c", "a", "b"} {
		req := basicVM(name)
		req.Disks, req.NICs = nil, nil
		f.create(req)
	}
	page, err := f.svc.VMs(ctx, models.VMListOptions{Limit: 2})
	if err != nil || len(page.Items) != 2 || page.Items[0].Name != "a" || page.Items[1].Name != "b" || page.NextCursor == "" {
		t.Fatalf("page 1 = %+v, %v", page, err)
	}
	page, err = f.svc.VMs(ctx, models.VMListOptions{Limit: 2, Cursor: page.NextCursor})
	if err != nil || len(page.Items) != 1 || page.Items[0].Name != "c" || page.NextCursor != "" {
		t.Fatalf("page 2 = %+v, %v", page, err)
	}
	if page, _ := f.svc.VMs(ctx, models.VMListOptions{State: models.VMRunning}); len(page.Items) != 0 {
		t.Errorf("running VMs = %+v", page.Items)
	}
	if page, _ := f.svc.VMs(ctx, models.VMListOptions{State: models.VMStopped}); len(page.Items) != 3 {
		t.Errorf("stopped VMs = %d", len(page.Items))
	}
	for _, opts := range []models.VMListOptions{{State: "sleeping"}, {Limit: 1000}, {Cursor: "!!"}} {
		if _, err := f.svc.VMs(ctx, opts); code(err) != models.CodeInvalidRequest {
			t.Errorf("%+v: %v", opts, err)
		}
	}
	media, err := f.svc.Media(ctx)
	if err != nil || len(media.Items) != 1 || media.Items[0].Name != testISO || media.Items[0].SizeBytes != 3 {
		t.Errorf("media = %+v, %v", media, err)
	}
	if caps := f.svc.Capabilities(ctx); !caps.Available || !caps.Simulated || caps.MaxVCPUsPerVM != 4 ||
		caps.GuestMemoryBytes != 5*gib || len(caps.Pools) != 1 {
		t.Errorf("capabilities = %+v", caps)
	}
}
