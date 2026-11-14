package vm

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/ids"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Definitions returns the task types of the VM lifecycle, with the time
// limits and cancellability of ADR 0009 decision 9.
func (s *Service) Definitions() []tasks.Definition {
	return append([]tasks.Definition{
		{Type: models.TaskVMCreate, Cancellable: true, Timeout: 10 * time.Minute, Run: s.runCreate},
		{Type: models.TaskVMUpdate, Timeout: time.Minute, Run: s.runUpdate},
		{Type: models.TaskVMDelete, Timeout: 10 * time.Minute, Run: s.runDelete},
	}, s.actionDefinitions()...)
}

// failure turns an error the user can act on into a task failure with its
// code. Anything else stays an internal error: the cause goes to the log.
func failure(err error) error {
	var me *models.Error
	if errors.As(err, &me) {
		return &tasks.Failure{Code: me.Code, Message: me.Message, Details: me.Details}
	}
	return err
}

// createPayload is the payload of a vm.create task: the request as it was
// normalised, before any ID was generated, so that a replay with the same
// idempotency key has the same payload.
type createPayload struct {
	Request models.VM `json:"request"`
}

// CreateVM validates a new VM, reserves its rows, and submits the task
// that provisions it. The VM exists, unprovisioned, as soon as this
// returns.
func (s *Service) CreateVM(ctx context.Context, req models.VMCreate, sub Submission) (models.VMCreated, error) {
	refused := sub.request(models.TaskVMCreate, "", nil)

	vm, err := normalizeCreate(req)
	if err != nil {
		return models.VMCreated{}, s.refuse(ctx, refused, err)
	}
	if vm.Engine != s.hv.Engine() {
		return models.VMCreated{}, s.refuse(ctx, refused, &models.Error{
			Code:    models.CodeEngineUnsupported,
			Message: "the engine " + vm.Engine + " is not supported; only " + s.hv.Engine() + " is",
		})
	}
	if _, err := s.hv.Validate(ctx, vm); err != nil {
		return models.VMCreated{}, s.refuse(ctx, refused, err)
	}
	for _, d := range media(vm) {
		if _, err := s.host.Files.ResolveMedia(ctx, d.Media); err != nil {
			return models.VMCreated{}, s.refuse(ctx, refused, err)
		}
	}
	raw, err := json.Marshal(createPayload{Request: vm})
	if err != nil {
		return models.VMCreated{}, err
	}
	payload, err := tasks.CanonicalPayload(raw)
	if err != nil {
		return models.VMCreated{}, err
	}

	// A replay finds the task it created the first time. The engine's own
	// replay check cannot: the VM ID is new on every call.
	if sub.IdempotencyKey != "" {
		existing, found, err := s.engine.LookupKey(ctx, sub.IdempotencyKey)
		if err != nil {
			return models.VMCreated{}, s.refuse(ctx, refused, err)
		}
		if found {
			if existing.Type != models.TaskVMCreate || existing.Actor.ID != sub.Actor.ID || !bytes.Equal(existing.Payload, payload) {
				return models.VMCreated{}, s.refuse(ctx, refused, &models.Error{
					Code:    models.CodeIdempotencyConflict,
					Message: "this idempotency key was already used for a different request",
				})
			}
			created := models.VMCreated{Task: existing.Public()}
			if created.VM, err = s.store.Get(ctx, existing.ResourceID); errors.Is(err, models.ErrNotFound) {
				// The VM was deleted, or its create failed and cleaned up.
				created.VM = vm
				created.VM.ID, created.VM.NodeID = existing.ResourceID, s.nodeID
			} else if err != nil {
				return models.VMCreated{}, err
			}
			return created, nil
		}
	}

	if err := s.assignIDs(&vm); err != nil {
		return models.VMCreated{}, err
	}
	submit := sub.request(models.TaskVMCreate, vm.ID, payload)
	submit.Companion = func(ctx context.Context, tx *sql.Tx) error {
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

// assignIDs gives a new VM, its disks, and its NICs their IDs, and every
// NIC without a MAC address a generated one.
func (s *Service) assignIDs(vm *models.VM) (err error) {
	vm.NodeID = s.nodeID
	vm.CreatedAt = s.now().UTC()
	vm.UpdatedAt = vm.CreatedAt
	if vm.ID, err = ids.NewUUID(); err != nil {
		return err
	}
	for i := range vm.Disks {
		if vm.Disks[i].ID, err = ids.NewUUID(); err != nil {
			return err
		}
	}
	for i := range vm.NICs {
		if vm.NICs[i].ID, err = ids.NewUUID(); err != nil {
			return err
		}
		if vm.NICs[i].MACAddress == "" {
			if vm.NICs[i].MACAddress, err = newMAC(); err != nil {
				return err
			}
		}
	}
	return nil
}

// DatasetInfo is one dataset in a task result or an audit entry.
type DatasetInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// SizeBytes is the volume size; UsedBytes is what the dataset used.
	SizeBytes int64 `json:"size_bytes,omitempty"`
	UsedBytes int64 `json:"used_bytes"`
}

func datasetInfo(list []hostops.Dataset) []DatasetInfo {
	out := make([]DatasetInfo, 0, len(list))
	for _, d := range list {
		out = append(out, DatasetInfo{Name: d.Name, Type: d.Type, SizeBytes: d.VolSizeBytes, UsedBytes: d.UsedBytes})
	}
	return out
}

// CreateResult is the result of a vm.create task.
type CreateResult struct {
	VMID     string        `json:"vm_id"`
	Name     string        `json:"name"`
	Datasets []DatasetInfo `json:"datasets"`
	// Warnings are things the user should know that did not stop the
	// create, such as thin disks that can outgrow the pool.
	Warnings []string `json:"warnings,omitempty"`
}

// AuditDetails implements tasks.Audited: the datasets created are on the
// record after the task is gone.
func (r CreateResult) AuditDetails() map[string]any {
	return map[string]any{"vm_name": r.Name, "datasets_created": r.Datasets}
}

func (s *Service) runCreate(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	vm, err := s.store.Get(ctx, task.ResourceID)
	if err != nil {
		return nil, err
	}
	if vm.Provisioned {
		return nil, &tasks.Failure{Code: models.CodeInvalidRequest, Message: "the virtual machine is already provisioned"}
	}
	// The host may have changed since the request was accepted.
	warnings, err := s.hv.Validate(ctx, vm)
	if err != nil {
		s.undoCreate(vm)
		return nil, failure(err)
	}
	report.Progress("creating the datasets and volumes", map[string]any{"disks": len(vm.Disks) - len(media(vm))})
	made, err := s.hv.Create(ctx, vm)
	if err != nil {
		// Failed or cancelled: take back what was made, so nothing is
		// left half-created. If that fails too, the VM stays, not
		// provisioned, and vm.delete removes what exists.
		s.undoCreate(vm)
		return nil, failure(err)
	}
	if err := s.saveVolumes(ctx, made.Datasets); err != nil {
		s.log.Warn("vm: could not record the observed volumes", "vm_id", vm.ID, "error", err)
	}
	if err := s.store.SetProvisioned(ctx, vm.ID, s.now().UTC()); err != nil {
		return nil, err
	}
	if _, err := s.Observe(ctx, vm.ID); err != nil {
		s.log.Warn("vm: could not observe the new VM", "vm_id", vm.ID, "error", err)
	}
	s.log.Info("vm created", "vm_id", vm.ID, "name", vm.Name, "datasets", len(made.Datasets))
	return CreateResult{VMID: vm.ID, Name: vm.Name, Datasets: datasetInfo(made.Datasets), Warnings: warnings}, nil
}

// undoCreate removes what a failed or cancelled vm.create left behind. It
// uses its own context: the task's may be the reason it is running.
func (s *Service) undoCreate(vm models.VM) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := s.hv.Delete(ctx, vm); err != nil {
		s.log.Error("vm: could not clean up after a failed create; the VM stays unprovisioned until it is deleted",
			"vm_id", vm.ID, "error", err)
		return
	}
	if err := s.store.Delete(ctx, vm.ID); err != nil {
		s.log.Error("vm: could not remove the rows of a failed create", "vm_id", vm.ID, "error", err)
	}
}

// saveVolumes records the volumes among the given datasets as observed.
func (s *Service) saveVolumes(ctx context.Context, datasets []hostops.Dataset) error {
	var list []database.VolumeObservation
	for _, d := range datasets {
		if d.Type != "volume" {
			continue
		}
		for _, pool := range s.host.Conf.Pools {
			if len(d.Name) > len(pool)+1 && d.Name[:len(pool)+1] == pool+"/" {
				list = append(list, database.VolumeObservation{
					Pool: pool, Name: d.Name[len(pool)+1:],
					VolSizeBytes: d.VolSizeBytes, UsedBytes: d.UsedBytes, ReferencedBytes: d.ReferencedBytes,
				})
			}
		}
	}
	return s.store.SaveVolumes(ctx, s.nodeID, list, s.now().UTC())
}

// stoppedVM returns a VM that exists and has nothing running on the host,
// which is what an update and a delete require.
func (s *Service) stoppedVM(ctx context.Context, id string) (models.VM, error) {
	vm, err := s.VM(ctx, id)
	if err != nil {
		return models.VM{}, err
	}
	obs, err := s.hv.Inspect(ctx, id)
	if err != nil {
		return models.VM{}, err
	}
	if obs.Active {
		return models.VM{}, &models.Error{
			Code:    models.CodeVMNotStopped,
			Message: "the virtual machine is " + obs.State + "; stop it first",
			Details: map[string]any{"state": obs.State},
		}
	}
	return vm, nil
}

// checkUpdate validates an update against the VM as it is now and returns
// the VM as the update would leave it.
func (s *Service) checkUpdate(ctx context.Context, id string, upd models.VMUpdate) (models.VM, []string, error) {
	vm, err := s.stoppedVM(ctx, id)
	if err != nil {
		return models.VM{}, nil, err
	}
	if !vm.Provisioned {
		return models.VM{}, nil, &models.Error{
			Code:    models.CodeVMNotProvisioned,
			Message: "the virtual machine was never provisioned; it can only be deleted",
		}
	}
	next, changed, err := applyUpdate(vm, upd)
	if err != nil {
		return models.VM{}, nil, err
	}
	if _, err := s.hv.Validate(ctx, next); err != nil {
		return models.VM{}, nil, err
	}
	if upd.Media != nil {
		for _, d := range media(next) {
			if _, err := s.host.Files.ResolveMedia(ctx, d.Media); err != nil {
				return models.VM{}, nil, err
			}
		}
	}
	return next, changed, nil
}

// UpdateVM submits a vm.update task. Only a stopped VM can be updated.
func (s *Service) UpdateVM(ctx context.Context, id string, upd models.VMUpdate, sub Submission) (models.Task, error) {
	if !hostops.ValidID(id) {
		return models.Task{}, fmt.Errorf("vm %q: %w", id, models.ErrNotFound)
	}
	payload, err := json.Marshal(upd)
	if err != nil {
		return models.Task{}, err
	}
	req := sub.request(models.TaskVMUpdate, id, payload)
	if _, _, err := s.checkUpdate(ctx, id, upd); err != nil {
		return models.Task{}, s.refuse(ctx, req, err)
	}
	task, _, err := s.engine.Submit(ctx, req)
	if err != nil {
		return models.Task{}, err
	}
	return task.Public(), nil
}

// UpdateResult is the result of a vm.update task.
type UpdateResult struct {
	VMID    string   `json:"vm_id"`
	Changed []string `json:"changed"`
}

// AuditDetails implements tasks.Audited.
func (r UpdateResult) AuditDetails() map[string]any {
	return map[string]any{"changed": r.Changed}
}

func (s *Service) runUpdate(ctx context.Context, task tasks.Task, _ tasks.Reporter) (any, error) {
	var upd models.VMUpdate
	if err := json.Unmarshal(task.Payload, &upd); err != nil {
		return nil, &tasks.Failure{Code: models.CodeInvalidRequest, Message: "the task payload is not a VM update"}
	}
	// Everything is checked again: the VM may have been started, or the
	// media removed, since the request was accepted.
	next, changed, err := s.checkUpdate(ctx, task.ResourceID, upd)
	if err != nil {
		return nil, failure(err)
	}
	change := database.VMChange{
		Description: &next.Description, CPUCount: &next.CPUCount, MemoryBytes: &next.MemoryBytes, Config: &next.Config,
	}
	if upd.Media != nil {
		attached := media(next)
		for i := range attached {
			if attached[i].ID, err = ids.NewUUID(); err != nil {
				return nil, err
			}
		}
		change.Media = &attached
	}
	if err := s.store.Update(ctx, next.ID, change, s.now().UTC()); err != nil {
		return nil, err
	}
	if changed == nil {
		changed = []string{}
	}
	s.log.Info("vm updated", "vm_id", next.ID, "changed", changed)
	return UpdateResult{VMID: next.ID, Changed: changed}, nil
}

// DeleteVM submits a vm.delete task. Only a stopped VM can be deleted; one
// that was never provisioned can be.
func (s *Service) DeleteVM(ctx context.Context, id string, sub Submission) (models.Task, error) {
	if !hostops.ValidID(id) {
		return models.Task{}, fmt.Errorf("vm %q: %w", id, models.ErrNotFound)
	}
	req := sub.request(models.TaskVMDelete, id, nil)
	if _, err := s.stoppedVM(ctx, id); err != nil {
		return models.Task{}, s.refuse(ctx, req, err)
	}
	task, _, err := s.engine.Submit(ctx, req)
	if err != nil {
		return models.Task{}, err
	}
	return task.Public(), nil
}

// DeleteResult is the result of a vm.delete task.
type DeleteResult struct {
	VMID     string        `json:"vm_id"`
	Name     string        `json:"name"`
	Datasets []DatasetInfo `json:"datasets"`
}

// AuditDetails implements tasks.Audited: what was destroyed, and how big
// it was, stays on the record.
func (r DeleteResult) AuditDetails() map[string]any {
	return map[string]any{"vm_name": r.Name, "datasets_destroyed": r.Datasets}
}

func (s *Service) runDelete(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	vm, err := s.stoppedVM(ctx, task.ResourceID)
	if err != nil {
		return nil, failure(err)
	}
	report.Progress("destroying the datasets and the VM's state", nil)
	gone, err := s.hv.Delete(ctx, vm)
	if err != nil {
		return nil, failure(err)
	}
	if err := s.store.Delete(ctx, vm.ID); err != nil {
		return nil, err
	}
	s.log.Info("vm deleted", "vm_id", vm.ID, "name", vm.Name, "datasets", len(gone.Datasets))
	return DeleteResult{VMID: vm.ID, Name: vm.Name, Datasets: datasetInfo(gone.Datasets)}, nil
}
