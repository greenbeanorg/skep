package vm

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/ids"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Snapshots, rollback, and linked clones of VMs (ADR 0010 decisions 2 and
// 3). They are storage operations, the same for every engine, so they use
// hostops' ZFS and Files directly rather than the hypervisor adapter; the
// adapter is asked only whether the VM runs.
//
// Every task holds the VM as its resource, so a snapshot cannot race a
// start, an update, a rollback, or a delete of the same VM. A clone holds
// its source; the clone itself is a new, unprovisioned VM that cannot be
// deleted while the clone task runs.

func (s *Service) snapshotDefinitions() []tasks.Definition {
	return []tasks.Definition{
		{Type: models.TaskVMSnapshotCreate, Cancellable: true, Timeout: time.Minute, Run: s.runSnapshotCreate},
		{Type: models.TaskVMSnapshotRollback, Timeout: 5 * time.Minute, Run: s.runSnapshotRollback},
		{Type: models.TaskVMSnapshotDelete, Timeout: 5 * time.Minute, Run: s.runSnapshotDelete},
		{Type: models.TaskVMClone, Timeout: 10 * time.Minute, Run: s.runClone},
	}
}

func vmOwner(id string) hostops.Owner { return hostops.Owner{Kind: hostops.OwnerVM, ID: id} }

// diskPools returns the pools of a VM's disks, in slot order.
func diskPools(vm models.VM) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range vm.Disks {
		if d.Bus != models.BusAHCICD && !seen[d.Pool] {
			seen[d.Pool] = true
			out = append(out, d.Pool)
		}
	}
	return out
}

// snapshotPool returns the one pool a VM's snapshot lives in. ZFS takes a
// snapshot atomically only within one pool, so a VM with disks in two
// pools cannot have a snapshot that is "one atomic snapshot of every
// disk" (decision 2a), and is refused rather than given a weaker one.
func snapshotPool(conf hostops.Confinement, vm models.VM) (string, error) {
	pools := diskPools(vm)
	switch len(pools) {
	case 0:
		return "", &models.Error{Code: models.CodeStorageFeatureUnsupported,
			Message: "the virtual machine has no disks to snapshot"}
	case 1:
		if conf.IsDirectory(pools[0]) {
			return "", &models.Error{Code: models.CodeStorageFeatureUnsupported,
				Message: "the virtual machine's disks are in the directory pool " + pools[0] +
					", which has no snapshots or clones; only ZFS pools do",
				Details: map[string]any{"pool": pools[0], "pool_type": hostops.PoolKindDirectory}}
		}
		return pools[0], nil
	}
	return "", &models.Error{Code: models.CodeStorageFeatureUnsupported,
		Message: "the virtual machine has disks in more than one pool, and ZFS snapshots are atomic only within a pool",
		Details: map[string]any{"pools": pools}}
}

var errSnapshotNotProvisioned = &models.Error{Code: models.CodeSnapshotNotProvisioned,
	Message: "the snapshot was never completed; it can only be deleted"}

func validSnapshotRequest(name, description string) error {
	switch {
	case !nameRE.MatchString(name):
		return models.Invalid("a snapshot name is 1 to 63 letters, digits, '.', '_', or '-', starting with a letter or digit")
	case len(description) > maxDescription:
		return models.Invalid(fmt.Sprintf("the description is longer than %d bytes", maxDescription))
	}
	return nil
}

// maxDescription bounds a snapshot's description, as the schema does.
const maxDescription = 1024

// snapshotPayload is the payload of every snapshot task. Request is what
// the user sent, normalised, so a replay with the same idempotency key
// can be recognised although the generated IDs differ.
type snapshotPayload struct {
	Request      json.RawMessage `json:"request,omitempty"`
	SnapshotID   string          `json:"snapshot_id"`
	DestroyNewer bool            `json:"destroy_newer,omitempty"`
}

// clonePayload is the payload of vm.clone.
type clonePayload struct {
	Request    json.RawMessage `json:"request"`
	TargetVMID string          `json:"target_vm_id"`
	SnapshotID string          `json:"snapshot_id"`
	// TakeSnapshot is set when the request named no snapshot: the task
	// takes one first and leaves it behind.
	TakeSnapshot bool `json:"take_snapshot,omitempty"`
}

func canonical(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return tasks.CanonicalPayload(raw)
}

// replay finds the task that an earlier request with the same idempotency
// key created, for tasks whose payload holds generated IDs beside the
// request. A key used for another request is idempotency_key_conflict.
func (s *Service) replay(ctx context.Context, sub Submission, taskType string, request json.RawMessage) (tasks.Task, bool, error) {
	if sub.IdempotencyKey == "" {
		return tasks.Task{}, false, nil
	}
	existing, found, err := s.engine.LookupKey(ctx, sub.IdempotencyKey)
	if err != nil || !found {
		return tasks.Task{}, false, err
	}
	var p struct {
		Request json.RawMessage `json:"request"`
	}
	same := existing.Type == taskType && existing.Actor.ID == sub.Actor.ID &&
		json.Unmarshal(existing.Payload, &p) == nil
	if same {
		earlier, err := tasks.CanonicalPayload(p.Request)
		same = err == nil && bytes.Equal(earlier, request)
	}
	if !same {
		return tasks.Task{}, false, &models.Error{Code: models.CodeIdempotencyConflict,
			Message: "this idempotency key was already used for a different request"}
	}
	return existing, true, nil
}

// consistency is what a snapshot taken now would be.
func (s *Service) consistency(ctx context.Context, vmID string) (string, error) {
	obs, err := s.hv.Inspect(ctx, vmID)
	if err != nil {
		return "", err
	}
	if obs.Active {
		return models.SnapshotCrashConsistent, nil
	}
	return models.SnapshotConsistent, nil
}

// provisionedVM returns a VM that vm.create has finished.
func (s *Service) provisionedVM(ctx context.Context, id string) (models.VM, error) {
	vm, err := s.VM(ctx, id)
	if err != nil {
		return models.VM{}, err
	}
	if !vm.Provisioned {
		return models.VM{}, &models.Error{Code: models.CodeVMNotProvisioned,
			Message: "the virtual machine was never provisioned; it can only be deleted"}
	}
	return vm, nil
}

// Snapshots lists a VM's snapshots, oldest first, with what ZFS shows of
// each now.
func (s *Service) Snapshots(ctx context.Context, vmID string) (models.SnapshotList, error) {
	vm, err := s.VM(ctx, vmID)
	if err != nil {
		return models.SnapshotList{}, err
	}
	list, err := s.snaps.List(ctx, models.VMResource, vm.ID)
	if err != nil {
		return models.SnapshotList{}, err
	}
	s.observeSnapshots(ctx, vm, list)
	return models.SnapshotList{Items: list}, nil
}

// Snapshot returns one snapshot of a VM.
func (s *Service) Snapshot(ctx context.Context, vmID, id string) (models.Snapshot, error) {
	if !hostops.ValidID(vmID) || !hostops.ValidID(id) {
		return models.Snapshot{}, fmt.Errorf("snapshot %q: %w", id, models.ErrNotFound)
	}
	vm, err := s.VM(ctx, vmID)
	if err != nil {
		return models.Snapshot{}, err
	}
	snap, err := s.snaps.Get(ctx, models.VMResource, vm.ID, id)
	if err != nil {
		return models.Snapshot{}, err
	}
	list := []models.Snapshot{snap}
	s.observeSnapshots(ctx, vm, list)
	return list[0], nil
}

// observeSnapshots fills in and records what ZFS shows of the given
// snapshots. A failure to read is logged: the answer is then the last
// observation.
func (s *Service) observeSnapshots(ctx context.Context, vm models.VM, list []models.Snapshot) {
	pool, err := snapshotPool(s.host.Conf, vm)
	if err != nil || len(list) == 0 {
		return
	}
	found, err := s.host.ZFS.Snapshots(ctx, pool, vmOwner(vm.ID))
	if err != nil {
		s.log.Warn("vm: could not read the snapshots of a VM", "vm_id", vm.ID, "error", err)
		return
	}
	used := map[string]int64{}
	exists := map[string]bool{}
	for _, d := range found {
		if d.SnapshotID != "" {
			exists[d.SnapshotID] = true
			used[d.SnapshotID] += d.UsedBytes
		}
	}
	at := s.now().UTC()
	for i := range list {
		id := list[i].ID
		list[i].Observed = &models.SnapshotObserved{Exists: exists[id], UsedBytes: used[id], At: &at}
		if err := s.snaps.SaveObserved(ctx, id, exists[id], used[id], at); err != nil {
			s.log.Warn("vm: could not record an observed snapshot", "snapshot_id", id, "error", err)
		}
	}
}

// CreateSnapshot validates a snapshot request, reserves its row, and
// submits the task that takes it.
func (s *Service) CreateSnapshot(ctx context.Context, vmID string, req models.SnapshotCreate, sub Submission) (models.SnapshotCreated, error) {
	if !hostops.ValidID(vmID) {
		return models.SnapshotCreated{}, fmt.Errorf("vm %q: %w", vmID, models.ErrNotFound)
	}
	refused := sub.request(models.TaskVMSnapshotCreate, vmID, nil)
	if err := validSnapshotRequest(req.Name, req.Description); err != nil {
		return models.SnapshotCreated{}, s.refuse(ctx, refused, err)
	}
	vm, err := s.provisionedVM(ctx, vmID)
	if err != nil {
		return models.SnapshotCreated{}, s.refuse(ctx, refused, err)
	}
	if _, err := snapshotPool(s.host.Conf, vm); err != nil {
		return models.SnapshotCreated{}, s.refuse(ctx, refused, err)
	}
	request, err := canonical(req)
	if err != nil {
		return models.SnapshotCreated{}, err
	}
	if existing, found, err := s.replay(ctx, sub, models.TaskVMSnapshotCreate, request); err != nil {
		return models.SnapshotCreated{}, s.refuse(ctx, refused, err)
	} else if found {
		var p snapshotPayload
		_ = json.Unmarshal(existing.Payload, &p)
		snap, err := s.snaps.Get(ctx, models.VMResource, vm.ID, p.SnapshotID)
		if errors.Is(err, models.ErrNotFound) {
			snap = models.Snapshot{ID: p.SnapshotID, ResourceKind: models.VMResource, ResourceID: vm.ID,
				Name: req.Name, Description: req.Description, Clones: []models.SnapshotClone{}}
		} else if err != nil {
			return models.SnapshotCreated{}, err
		}
		return models.SnapshotCreated{Snapshot: snap, Task: existing.Public()}, nil
	}
	consistency, err := s.consistency(ctx, vm.ID)
	if err != nil {
		return models.SnapshotCreated{}, err
	}
	snap := models.Snapshot{ResourceKind: models.VMResource, ResourceID: vm.ID, Name: req.Name,
		Description: req.Description, Consistency: consistency, CreatedAt: s.now().UTC()}
	if snap.ID, err = ids.NewUUID(); err != nil {
		return models.SnapshotCreated{}, err
	}
	payload, err := canonical(snapshotPayload{Request: request, SnapshotID: snap.ID})
	if err != nil {
		return models.SnapshotCreated{}, err
	}
	submit := sub.request(models.TaskVMSnapshotCreate, vm.ID, payload)
	submit.Companion = func(ctx context.Context, tx *sql.Tx) error {
		return s.snaps.InsertTx(ctx, tx, snap, s.maxSnapshots)
	}
	task, _, err := s.engine.Submit(ctx, submit)
	if err != nil {
		return models.SnapshotCreated{}, err
	}
	stored, err := s.snaps.Get(ctx, models.VMResource, vm.ID, snap.ID)
	if err != nil {
		return models.SnapshotCreated{}, err
	}
	return models.SnapshotCreated{Snapshot: stored, Task: task.Public()}, nil
}

func decodeSnapshotPayload(task tasks.Task) (snapshotPayload, error) {
	var p snapshotPayload
	if err := json.Unmarshal(task.Payload, &p); err != nil || !hostops.ValidID(p.SnapshotID) {
		return p, &tasks.Failure{Code: models.CodeInvalidRequest, Message: "the task payload is not a snapshot request"}
	}
	return p, nil
}

// SnapshotResult is the result of vm.snapshot.create.
type SnapshotResult struct {
	VMID        string        `json:"vm_id"`
	SnapshotID  string        `json:"snapshot_id"`
	Name        string        `json:"name"`
	Consistency string        `json:"consistency"`
	Datasets    []DatasetInfo `json:"datasets"`
}

// AuditDetails implements tasks.Audited.
func (r SnapshotResult) AuditDetails() map[string]any {
	return map[string]any{"snapshot_id": r.SnapshotID, "snapshot_name": r.Name, "consistency": r.Consistency,
		"snapshots_created": r.Datasets}
}

func (s *Service) runSnapshotCreate(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	p, err := decodeSnapshotPayload(task)
	if err != nil {
		return nil, err
	}
	snap, err := s.snaps.Get(ctx, models.VMResource, task.ResourceID, p.SnapshotID)
	if err != nil {
		return nil, err
	}
	if snap.Provisioned {
		return nil, &tasks.Failure{Code: models.CodeInvalidRequest, Message: "the snapshot was already taken"}
	}
	vm, err := s.provisionedVM(ctx, task.ResourceID)
	if err != nil {
		s.dropSnapshotRow(snap.ID)
		return nil, failure(err)
	}
	made, consistency, err := s.takeSnapshot(ctx, vm, snap.ID, report)
	if err != nil {
		s.dropSnapshotRow(snap.ID)
		return nil, failure(err)
	}
	s.log.Info("vm snapshot taken", "vm_id", vm.ID, "snapshot_id", snap.ID, "consistency", consistency)
	return SnapshotResult{VMID: vm.ID, SnapshotID: snap.ID, Name: snap.Name, Consistency: consistency,
		Datasets: datasetInfo(made)}, nil
}

// takeSnapshot copies the variable store, takes the ZFS snapshot, and
// marks the row provisioned. It can be cancelled until the ZFS call;
// after that the call runs to its end. Whatever fails is undone here,
// except the row, which is the caller's.
func (s *Service) takeSnapshot(ctx context.Context, vm models.VM, snapID string, report tasks.Reporter) ([]hostops.Dataset, string, error) {
	pool, err := snapshotPool(s.host.Conf, vm)
	if err != nil {
		return nil, "", err
	}
	// Taken when the snapshot is, not when it was asked for.
	consistency, err := s.consistency(ctx, vm.ID)
	if err != nil {
		return nil, "", err
	}
	report.Progress("copying the UEFI variable store", nil)
	if err := s.host.Files.SaveVarsSnapshot(ctx, vm.ID, snapID); err != nil {
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		s.removeVars(vm.ID, snapID)
		return nil, "", err
	}
	report.Progress("taking the ZFS snapshot", map[string]any{"pool": pool, "consistency": consistency})
	made, err := s.host.ZFS.Snapshot(context.WithoutCancel(ctx), pool, vmOwner(vm.ID), snapID)
	if err != nil {
		s.removeVars(vm.ID, snapID)
		return nil, "", err
	}
	var used int64
	for _, d := range made {
		used += d.UsedBytes
	}
	if err := s.snaps.SetProvisioned(context.WithoutCancel(ctx), snapID, consistency, used, s.now().UTC()); err != nil {
		return nil, "", err
	}
	return made, consistency, nil
}

func (s *Service) removeVars(vmID, snapID string) {
	if err := s.host.Files.RemoveVarsSnapshot(vmID, snapID); err != nil {
		s.log.Warn("vm: could not remove a snapshot's variable store copy", "vm_id", vmID, "snapshot_id", snapID, "error", err)
	}
}

// dropSnapshotRow removes the row of a snapshot that was not taken. It
// uses its own context: the task's may be why it is running.
func (s *Service) dropSnapshotRow(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.snaps.Delete(ctx, id); err != nil {
		s.log.Error("vm: could not remove the row of a snapshot that was not taken", "snapshot_id", id, "error", err)
	}
}

// clonesError is snapshot_has_clones from the database's records.
func clonesError(clones []models.SnapshotClone) error {
	return &models.Error{Code: models.CodeSnapshotHasClones,
		Message: "a linked clone depends on the snapshot; delete the clone first",
		Details: map[string]any{"clones": clones}}
}

// checkRollback validates a rollback against the database: the VM is
// stopped and provisioned, the snapshot is complete, and the newer
// snapshots may be destroyed. ZFS checks again in the task.
func (s *Service) checkRollback(ctx context.Context, vmID, snapID string, destroyNewer bool) (models.VM, models.Snapshot, []models.Snapshot, error) {
	vm, err := s.stoppedVM(ctx, vmID)
	if err != nil {
		return models.VM{}, models.Snapshot{}, nil, err
	}
	if !vm.Provisioned {
		return models.VM{}, models.Snapshot{}, nil, &models.Error{Code: models.CodeVMNotProvisioned,
			Message: "the virtual machine was never provisioned; it can only be deleted"}
	}
	if _, err := snapshotPool(s.host.Conf, vm); err != nil {
		return models.VM{}, models.Snapshot{}, nil, err
	}
	list, err := s.snaps.List(ctx, models.VMResource, vm.ID)
	if err != nil {
		return models.VM{}, models.Snapshot{}, nil, err
	}
	var target models.Snapshot
	var newer []models.Snapshot
	found := false
	for _, snap := range list {
		switch {
		case snap.ID == snapID:
			target, found = snap, true
		case found:
			newer = append(newer, snap)
		}
	}
	switch {
	case !found:
		return models.VM{}, models.Snapshot{}, nil, fmt.Errorf("snapshot %q: %w", snapID, models.ErrNotFound)
	case !target.Provisioned:
		return models.VM{}, models.Snapshot{}, nil, errSnapshotNotProvisioned
	case len(newer) > 0 && !destroyNewer:
		ids := make([]string, len(newer))
		for i, n := range newer {
			ids[i] = n.ID
		}
		return models.VM{}, models.Snapshot{}, nil, &models.Error{Code: models.CodeSnapshotNotLatest,
			Message: "the snapshot is not the latest; rolling back to it destroys the newer ones, which needs destroy_newer",
			Details: map[string]any{"snapshot_id": snapID, "newer_snapshot_ids": ids}}
	}
	var clones []models.SnapshotClone
	for _, n := range newer {
		clones = append(clones, n.Clones...)
	}
	if len(clones) > 0 {
		return models.VM{}, models.Snapshot{}, nil, clonesError(clones)
	}
	return vm, target, newer, nil
}

// RollbackSnapshot submits a vm.snapshot.rollback task.
func (s *Service) RollbackSnapshot(ctx context.Context, vmID, snapID string, req models.SnapshotRollback, sub Submission) (models.Task, error) {
	if !hostops.ValidID(vmID) || !hostops.ValidID(snapID) {
		return models.Task{}, fmt.Errorf("snapshot %q: %w", snapID, models.ErrNotFound)
	}
	payload, err := canonical(snapshotPayload{SnapshotID: snapID, DestroyNewer: req.DestroyNewer})
	if err != nil {
		return models.Task{}, err
	}
	submit := sub.request(models.TaskVMSnapshotRollback, vmID, payload)
	if _, _, _, err := s.checkRollback(ctx, vmID, snapID, req.DestroyNewer); err != nil {
		return models.Task{}, s.refuse(ctx, submit, err)
	}
	task, _, err := s.engine.Submit(ctx, submit)
	if err != nil {
		return models.Task{}, err
	}
	return task.Public(), nil
}

// SnapshotRef names a snapshot in a task result.
type SnapshotRef struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// RollbackResult is the result of vm.snapshot.rollback. Destroyed lists
// the newer snapshots destroyed with destroy_newer; DatasetsDestroyed the
// ZFS snapshots that were.
type RollbackResult struct {
	VMID              string        `json:"vm_id"`
	SnapshotID        string        `json:"snapshot_id"`
	Name              string        `json:"name"`
	Destroyed         []SnapshotRef `json:"destroyed_snapshots"`
	DatasetsDestroyed []DatasetInfo `json:"datasets_destroyed"`
}

// AuditDetails implements tasks.Audited: every snapshot a rollback
// destroyed is on the record (decision 14).
func (r RollbackResult) AuditDetails() map[string]any {
	return map[string]any{"snapshot_id": r.SnapshotID, "snapshot_name": r.Name,
		"snapshots_destroyed": r.Destroyed, "datasets_destroyed": r.DatasetsDestroyed}
}

func (s *Service) runSnapshotRollback(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	p, err := decodeSnapshotPayload(task)
	if err != nil {
		return nil, err
	}
	vm, target, newer, err := s.checkRollback(ctx, task.ResourceID, p.SnapshotID, p.DestroyNewer)
	if err != nil {
		return nil, failure(err)
	}
	pool, _ := snapshotPool(s.host.Conf, vm)
	report.Progress("rolling the disks back", map[string]any{"snapshot_id": target.ID, "destroy_newer": p.DestroyNewer})
	destroyed, err := s.host.ZFS.Rollback(ctx, pool, vmOwner(vm.ID), target.ID, p.DestroyNewer)
	if err != nil {
		return nil, failure(err)
	}
	report.Progress("restoring the UEFI variable store", nil)
	if err := s.host.Files.RestoreVarsSnapshot(ctx, vm.ID, target.ID); err != nil {
		return nil, err
	}
	// What ZFS destroyed decides which rows go; the database's list of
	// newer snapshots names them.
	names := map[string]string{}
	for _, n := range newer {
		names[n.ID] = n.Name
	}
	gone := []SnapshotRef{}
	seen := map[string]bool{}
	for _, d := range destroyed {
		if d.SnapshotID == "" || seen[d.SnapshotID] {
			continue
		}
		seen[d.SnapshotID] = true
		gone = append(gone, SnapshotRef{ID: d.SnapshotID, Name: names[d.SnapshotID]})
		s.removeVars(vm.ID, d.SnapshotID)
		if err := s.snaps.Delete(ctx, d.SnapshotID); err != nil {
			return nil, err
		}
	}
	s.refreshVolumes(ctx, vm)
	s.log.Info("vm rolled back", "vm_id", vm.ID, "snapshot_id", target.ID, "destroyed", len(gone))
	return RollbackResult{VMID: vm.ID, SnapshotID: target.ID, Name: target.Name, Destroyed: gone,
		DatasetsDestroyed: datasetInfo(destroyed)}, nil
}

// refreshVolumes records what the VM's volumes look like now, and makes
// each disk's recorded size the zvol's: a rollback, or a clone, of a
// snapshot taken before a disk grew gives the disk its earlier size.
func (s *Service) refreshVolumes(ctx context.Context, vm models.VM) {
	for _, pool := range diskPools(vm) {
		list, err := s.host.Disks().VMVolumes(ctx, pool, vm.ID)
		if err == nil {
			err = s.saveVolumes(ctx, list)
		}
		if err == nil {
			err = s.syncDiskSizes(ctx, vm, list)
		}
		if err != nil {
			s.log.Warn("vm: could not record the VM's volumes", "vm_id", vm.ID, "error", err)
		}
	}
}

func (s *Service) syncDiskSizes(ctx context.Context, vm models.VM, list []hostops.Dataset) error {
	byName := map[string]hostops.Dataset{}
	for _, d := range list {
		byName[d.Name] = d
	}
	for _, d := range vm.Disks {
		z, ok := byName[d.Dataset]
		if d.ID == "" || !ok || z.Type != "volume" || z.VolSizeBytes == d.SizeBytes || z.VolSizeBytes <= 0 {
			continue
		}
		s.log.Info("vm: a disk's size follows its zvol", "vm_id", vm.ID, "slot", d.Slot, "from", d.SizeBytes, "to", z.VolSizeBytes)
		if err := s.store.ResizeDisk(ctx, vm.ID, d.ID, z.VolSizeBytes, s.now().UTC()); err != nil {
			return err
		}
	}
	return nil
}

// checkSnapshotDelete validates a snapshot delete: the snapshot exists and
// no clone depends on it. The VM may be running.
func (s *Service) checkSnapshotDelete(ctx context.Context, vmID, snapID string) (models.VM, models.Snapshot, error) {
	vm, err := s.VM(ctx, vmID)
	if err != nil {
		return models.VM{}, models.Snapshot{}, err
	}
	snap, err := s.snaps.Get(ctx, models.VMResource, vm.ID, snapID)
	if err != nil {
		return models.VM{}, models.Snapshot{}, err
	}
	if len(snap.Clones) > 0 {
		return models.VM{}, models.Snapshot{}, clonesError(snap.Clones)
	}
	return vm, snap, nil
}

// DeleteSnapshot submits a vm.snapshot.delete task.
func (s *Service) DeleteSnapshot(ctx context.Context, vmID, snapID string, sub Submission) (models.Task, error) {
	if !hostops.ValidID(vmID) || !hostops.ValidID(snapID) {
		return models.Task{}, fmt.Errorf("snapshot %q: %w", snapID, models.ErrNotFound)
	}
	payload, err := canonical(snapshotPayload{SnapshotID: snapID})
	if err != nil {
		return models.Task{}, err
	}
	submit := sub.request(models.TaskVMSnapshotDelete, vmID, payload)
	if _, _, err := s.checkSnapshotDelete(ctx, vmID, snapID); err != nil {
		return models.Task{}, s.refuse(ctx, submit, err)
	}
	task, _, err := s.engine.Submit(ctx, submit)
	if err != nil {
		return models.Task{}, err
	}
	return task.Public(), nil
}

// SnapshotDeleteResult is the result of vm.snapshot.delete.
type SnapshotDeleteResult struct {
	VMID       string        `json:"vm_id"`
	SnapshotID string        `json:"snapshot_id"`
	Name       string        `json:"name"`
	Datasets   []DatasetInfo `json:"datasets_destroyed"`
}

// AuditDetails implements tasks.Audited.
func (r SnapshotDeleteResult) AuditDetails() map[string]any {
	return map[string]any{"snapshot_id": r.SnapshotID, "snapshot_name": r.Name, "datasets_destroyed": r.Datasets}
}

func (s *Service) runSnapshotDelete(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	p, err := decodeSnapshotPayload(task)
	if err != nil {
		return nil, err
	}
	vm, snap, err := s.checkSnapshotDelete(ctx, task.ResourceID, p.SnapshotID)
	if err != nil {
		return nil, failure(err)
	}
	report.Progress("destroying the snapshot", nil)
	var destroyed []hostops.Dataset
	// Every pool the VM uses: a snapshot of a VM that has since gained a
	// disk elsewhere still goes everywhere it is.
	for _, pool := range diskPools(vm) {
		if s.host.Conf.IsDirectory(pool) {
			continue
		}
		gone, err := s.host.ZFS.DestroySnapshot(ctx, pool, vmOwner(vm.ID), snap.ID)
		if errors.Is(err, models.ErrNotFound) {
			// Not in ZFS (an interrupted snapshot task): only the row is
			// left to remove.
			continue
		}
		if err != nil {
			return nil, failure(err)
		}
		destroyed = append(destroyed, gone...)
	}
	s.removeVars(vm.ID, snap.ID)
	if err := s.snaps.Delete(ctx, snap.ID); err != nil {
		return nil, err
	}
	s.log.Info("vm snapshot deleted", "vm_id", vm.ID, "snapshot_id", snap.ID, "datasets", len(destroyed))
	return SnapshotDeleteResult{VMID: vm.ID, SnapshotID: snap.ID, Name: snap.Name, Datasets: datasetInfo(destroyed)}, nil
}

// cloneSource validates the source of a clone and, when named, its
// snapshot.
func (s *Service) cloneSource(ctx context.Context, srcID, snapID string) (models.VM, error) {
	src, err := s.provisionedVM(ctx, srcID)
	if err != nil {
		return models.VM{}, err
	}
	if _, err := snapshotPool(s.host.Conf, src); err != nil {
		return models.VM{}, err
	}
	if snapID != "" {
		if !hostops.ValidID(snapID) {
			return models.VM{}, models.Invalid("invalid snapshot ID")
		}
		snap, err := s.snaps.Get(ctx, models.VMResource, src.ID, snapID)
		if errors.Is(err, models.ErrNotFound) {
			return models.VM{}, models.Invalid("the source VM has no snapshot " + snapID)
		}
		if err != nil {
			return models.VM{}, err
		}
		if !snap.Provisioned {
			return models.VM{}, errSnapshotNotProvisioned
		}
	}
	return src, nil
}

// cloneOf builds the clone of a VM: the source's configuration and disk
// layout, a new name, and, when IDs are assigned, new IDs and MAC
// addresses. Install media is not attached: a clone boots from its disks.
// Its disks are thin, as every linked clone is.
func cloneOf(src models.VM, req models.VMClone) models.VM {
	vm := models.VM{
		Name: req.Name, Description: req.Description, Engine: src.Engine, CPUCount: src.CPUCount,
		MemoryBytes: src.MemoryBytes, Firmware: src.Firmware, Config: src.Config,
		Disks: []models.VMDisk{}, NICs: []models.VMNIC{},
	}
	for _, d := range src.Disks {
		if d.Bus == models.BusAHCICD {
			continue
		}
		vm.Disks = append(vm.Disks, models.VMDisk{Slot: d.Slot, Bus: d.Bus, SizeBytes: d.SizeBytes, Pool: d.Pool,
			Thin: true, VolBlockSize: d.VolBlockSize})
	}
	for _, n := range src.NICs {
		vm.NICs = append(vm.NICs, models.VMNIC{Slot: n.Slot, Model: n.Model, Bridge: n.Bridge})
	}
	return vm
}

// CloneVM validates a clone request, reserves the clone's rows (and the
// snapshot's, when it takes one), and submits the vm.clone task.
func (s *Service) CloneVM(ctx context.Context, srcID string, req models.VMClone, sub Submission) (models.VMCreated, error) {
	if !hostops.ValidID(srcID) {
		return models.VMCreated{}, fmt.Errorf("vm %q: %w", srcID, models.ErrNotFound)
	}
	refused := sub.request(models.TaskVMClone, srcID, nil)
	if err := validSnapshotRequest(req.Name, req.Description); err != nil {
		return models.VMCreated{}, s.refuse(ctx, refused, err)
	}
	src, err := s.cloneSource(ctx, srcID, req.SnapshotID)
	if err != nil {
		return models.VMCreated{}, s.refuse(ctx, refused, err)
	}
	vm := cloneOf(src, req)
	if _, err := s.hv.Validate(ctx, vm); err != nil {
		return models.VMCreated{}, s.refuse(ctx, refused, err)
	}
	request, err := canonical(req)
	if err != nil {
		return models.VMCreated{}, err
	}
	if existing, found, err := s.replay(ctx, sub, models.TaskVMClone, request); err != nil {
		return models.VMCreated{}, s.refuse(ctx, refused, err)
	} else if found {
		var p clonePayload
		_ = json.Unmarshal(existing.Payload, &p)
		created := models.VMCreated{Task: existing.Public()}
		if created.VM, err = s.store.Get(ctx, p.TargetVMID); errors.Is(err, models.ErrNotFound) {
			created.VM = vm
			created.VM.ID, created.VM.NodeID = p.TargetVMID, s.nodeID
		} else if err != nil {
			return models.VMCreated{}, err
		}
		return created, nil
	}

	if err := s.assignIDs(&vm); err != nil {
		return models.VMCreated{}, err
	}
	p := clonePayload{Request: request, TargetVMID: vm.ID, SnapshotID: req.SnapshotID}
	var snap models.Snapshot
	if p.SnapshotID == "" {
		p.TakeSnapshot = true
		consistency, err := s.consistency(ctx, src.ID)
		if err != nil {
			return models.VMCreated{}, err
		}
		snap = models.Snapshot{ResourceKind: models.VMResource, ResourceID: src.ID, Name: req.Name,
			Description: "taken for the clone " + req.Name, Consistency: consistency, CreatedAt: s.now().UTC()}
		if snap.ID, err = ids.NewUUID(); err != nil {
			return models.VMCreated{}, err
		}
		p.SnapshotID = snap.ID
	}
	vm.ClonedFromSnapshotID = p.SnapshotID
	payload, err := canonical(p)
	if err != nil {
		return models.VMCreated{}, err
	}
	submit := sub.request(models.TaskVMClone, src.ID, payload)
	submit.Companion = func(ctx context.Context, tx *sql.Tx) error {
		if p.TakeSnapshot {
			if err := s.snaps.InsertTx(ctx, tx, snap, s.maxSnapshots); err != nil {
				return err
			}
		}
		return s.store.InsertTx(ctx, tx, vm, s.host.Conf.DatasetRoot)
	}
	task, _, err := s.engine.Submit(ctx, submit)
	if err != nil {
		return models.VMCreated{}, err
	}
	stored, err := s.store.Get(ctx, vm.ID)
	if err != nil {
		return models.VMCreated{}, err
	}
	return models.VMCreated{VM: stored, Task: task.Public()}, nil
}

// CloneResult is the result of vm.clone.
type CloneResult struct {
	VMID          string        `json:"vm_id"`
	Name          string        `json:"name"`
	SourceVMID    string        `json:"source_vm_id"`
	SnapshotID    string        `json:"snapshot_id"`
	SnapshotTaken bool          `json:"snapshot_taken"`
	Datasets      []DatasetInfo `json:"datasets"`
}

// AuditDetails implements tasks.Audited.
func (r CloneResult) AuditDetails() map[string]any {
	return map[string]any{"clone_vm_id": r.VMID, "clone_name": r.Name, "snapshot_id": r.SnapshotID,
		"snapshot_taken": r.SnapshotTaken, "datasets_created": r.Datasets}
}

func (s *Service) runClone(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	var p clonePayload
	if err := json.Unmarshal(task.Payload, &p); err != nil || !hostops.ValidID(p.TargetVMID) || !hostops.ValidID(p.SnapshotID) {
		return nil, &tasks.Failure{Code: models.CodeInvalidRequest, Message: "the task payload is not a clone request"}
	}
	target, err := s.store.Get(ctx, p.TargetVMID)
	if err != nil {
		return nil, err
	}
	if target.Provisioned {
		return nil, &tasks.Failure{Code: models.CodeInvalidRequest, Message: "the clone is already provisioned"}
	}
	src, err := s.provisionedVM(ctx, task.ResourceID)
	if err != nil {
		s.undoClone(target)
		return nil, failure(err)
	}
	if p.TakeSnapshot {
		if _, _, err := s.takeSnapshot(ctx, src, p.SnapshotID, report); err != nil {
			s.undoClone(target)
			s.dropSnapshotRow(p.SnapshotID)
			return nil, failure(err)
		}
	}
	if _, err := s.cloneSource(ctx, src.ID, p.SnapshotID); err != nil {
		s.undoClone(target)
		return nil, failure(err)
	}
	pool, _ := snapshotPool(s.host.Conf, src)
	disks := map[int]string{}
	for _, d := range target.Disks {
		if d.Bus != models.BusAHCICD {
			disks[d.Slot] = d.ID
		}
	}
	report.Progress("cloning the disks", map[string]any{"snapshot_id": p.SnapshotID, "disks": len(disks)})
	made, err := s.host.ZFS.CloneVM(ctx, hostops.CloneSpec{Pool: pool, SourceVMID: src.ID, SnapshotID: p.SnapshotID,
		TargetVMID: target.ID, Disks: disks})
	if err == nil {
		report.Progress("copying the UEFI variable store", nil)
		err = s.host.Files.CloneVars(ctx, src.ID, p.SnapshotID, target.ID)
	}
	if err != nil {
		s.undoClone(target)
		return nil, failure(err)
	}
	s.refreshVolumes(ctx, target)
	if err := s.store.SetProvisioned(ctx, target.ID, s.now().UTC()); err != nil {
		return nil, err
	}
	if _, err := s.Observe(ctx, target.ID); err != nil {
		s.log.Warn("vm: could not observe the new clone", "vm_id", target.ID, "error", err)
	}
	s.log.Info("vm cloned", "vm_id", target.ID, "source_vm_id", src.ID, "snapshot_id", p.SnapshotID)
	return CloneResult{VMID: target.ID, Name: target.Name, SourceVMID: src.ID, SnapshotID: p.SnapshotID,
		SnapshotTaken: p.TakeSnapshot, Datasets: datasetInfo(made)}, nil
}

// undoClone removes a clone that failed: its datasets, which carry its
// own owner property, its state directory, and its rows. A snapshot the
// task took stays: it is a valid snapshot of the source.
func (s *Service) undoClone(vm models.VM) { s.undoCreate(vm) }
