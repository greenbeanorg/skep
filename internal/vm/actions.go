package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
)

// actionDefinitions are the power tasks of ADR 0009 decision 9. None of
// them can be cancelled while it runs: a half-started VM is the user's to
// look at, not something a cancel should leave behind.
func (s *Service) actionDefinitions() []tasks.Definition {
	return []tasks.Definition{
		{Type: models.TaskVMStart, Timeout: 2 * time.Minute, Run: s.runStart},
		{Type: models.TaskVMStop, Timeout: 5 * time.Minute, Run: s.runStop},
		{Type: models.TaskVMForceStop, Timeout: time.Minute, Run: s.runForceStop},
		{Type: models.TaskVMRestart, Timeout: 7 * time.Minute, Run: s.runRestart},
	}
}

var actionTasks = map[string]string{
	models.VMActionStart:     models.TaskVMStart,
	models.VMActionStop:      models.TaskVMStop,
	models.VMActionForceStop: models.TaskVMForceStop,
	models.VMActionRestart:   models.TaskVMRestart,
}

// actionPayload is the payload of a power task.
type actionPayload struct {
	// TimeoutSeconds is how long a stop waits for the guest.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

func (p actionPayload) timeout() time.Duration {
	if p.TimeoutSeconds <= 0 {
		return models.DefaultStopTimeoutSeconds * time.Second
	}
	return time.Duration(p.TimeoutSeconds) * time.Second
}

// Action submits the power task an action asks for. What the state of the
// VM or the host refuses is refused here, audited, before a task exists;
// the task checks again when it runs.
func (s *Service) Action(ctx context.Context, id string, act models.VMAction, sub Submission) (models.Task, error) {
	if !hostops.ValidID(id) {
		return models.Task{}, fmt.Errorf("vm %q: %w", id, models.ErrNotFound)
	}
	taskType, ok := actionTasks[act.Action]
	if !ok {
		return models.Task{}, s.refuse(ctx, sub.request("vm.action", id, nil),
			models.Invalid(`action must be "start", "stop", "force_stop", or "restart"`))
	}
	var p actionPayload
	req := sub.request(taskType, id, nil)
	if act.Timeout != nil {
		switch {
		case taskType != models.TaskVMStop && taskType != models.TaskVMRestart:
			return models.Task{}, s.refuse(ctx, req, models.Invalid("timeout applies to stop and restart only"))
		case *act.Timeout < 1 || *act.Timeout > models.MaxStopTimeoutSeconds:
			return models.Task{}, s.refuse(ctx, req, models.Invalid(fmt.Sprintf("timeout must be between 1 and %d seconds", models.MaxStopTimeoutSeconds)))
		}
		p.TimeoutSeconds = *act.Timeout
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return models.Task{}, err
	}
	req.Payload = payload
	if err := s.checkAction(ctx, id, taskType); err != nil {
		return models.Task{}, s.refuse(ctx, req, err)
	}
	task, _, err := s.engine.Submit(ctx, req)
	if err != nil {
		return models.Task{}, err
	}
	return task.Public(), nil
}

// checkAction refuses an action the VM's state or the host does not
// allow now.
func (s *Service) checkAction(ctx context.Context, id, taskType string) error {
	vm, err := s.VM(ctx, id)
	if err != nil {
		return err
	}
	if !vm.Provisioned {
		return &models.Error{Code: models.CodeVMNotProvisioned,
			Message: "the virtual machine was never provisioned; it can only be deleted"}
	}
	obs, err := s.hv.Inspect(ctx, id)
	if err != nil {
		return err
	}
	switch taskType {
	case models.TaskVMStart:
		if obs.Active {
			return &models.Error{Code: models.CodeVMNotStopped,
				Message: "the virtual machine is " + obs.State, Details: map[string]any{"state": obs.State}}
		}
		if caps := s.hv.Capabilities(ctx); !caps.Available {
			return &models.Error{Code: models.CodeBhyveUnavailable, Message: "bhyve cannot run a VM on this host: " + caps.Reason}
		}
		if err := s.checkUnlocked(ctx, vm); err != nil {
			return err
		}
		return s.checkMemory(ctx, vm)
	default:
		if !obs.Active {
			return &models.Error{Code: models.CodeVMNotRunning,
				Message: "the virtual machine is " + obs.State, Details: map[string]any{"state": obs.State}}
		}
	}
	return nil
}

// checkUnlocked refuses to start a VM with a ZFS dataset whose encryption
// key is not loaded (ADR 0010 decision 5): its zvols have no device, and
// bhyve would fail to open them. Skep holds no key and loads none.
func (s *Service) checkUnlocked(ctx context.Context, vm models.VM) error {
	seen := map[string]bool{}
	for _, d := range vm.Disks {
		if d.Bus == models.BusAHCICD || d.Pool == "" || d.PoolType == hostops.PoolKindDirectory || seen[d.Pool] {
			continue
		}
		seen[d.Pool] = true
		list, err := s.host.Disks().VMVolumes(ctx, d.Pool, vm.ID)
		if err != nil {
			return err
		}
		for _, ds := range list {
			if ds.Locked() {
				return &models.Error{
					Code:    models.CodeStorageLocked,
					Message: "the encryption key of " + ds.Name + " is not loaded; an administrator loads it, Skep does not",
					Details: map[string]any{"dataset": ds.Name},
				}
			}
		}
	}
	return nil
}

// checkMemory is the memory budget (ADR 0009 decision 9): the VMs that
// run, and this one, within physical memory less the reserve.
func (s *Service) checkMemory(ctx context.Context, vm models.VM) error {
	caps := s.hv.Capabilities(ctx)
	active, err := s.store.ActiveMemory(ctx, vm.ID)
	if err != nil {
		return err
	}
	if active+vm.MemoryBytes > caps.GuestMemoryBytes {
		return &models.Error{Code: models.CodeInsufficientMemory,
			Message: fmt.Sprintf("the VMs that run use %d bytes and this one needs %d; the host allows %d for guests (physical memory less the configured reserve)",
				active, vm.MemoryBytes, caps.GuestMemoryBytes),
			Details: map[string]any{"running_bytes": active, "required_bytes": vm.MemoryBytes, "guest_memory_bytes": caps.GuestMemoryBytes}}
	}
	return nil
}

// ActionResult is the result of a power task: what the VM was observed
// to be when the task ended.
type ActionResult struct {
	VMID   string `json:"vm_id"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	PID    int    `json:"pid,omitempty"`
}

func (s *Service) actionTask(ctx context.Context, task tasks.Task, desired string,
	act func(models.VM, actionPayload) error) (any, error) {
	var p actionPayload
	if len(task.Payload) > 0 {
		if err := json.Unmarshal(task.Payload, &p); err != nil {
			return nil, &tasks.Failure{Code: models.CodeInvalidRequest, Message: "the task payload is not a VM action"}
		}
	}
	if err := s.checkAction(ctx, task.ResourceID, task.Type); err != nil {
		return nil, failure(err)
	}
	vm, err := s.store.Get(ctx, task.ResourceID)
	if err != nil {
		return nil, err
	}
	// What the user asked for is recorded first, whatever happens next
	// (ADR 0009 decision 2c).
	if err := s.store.SetDesiredState(ctx, vm.ID, desired, s.now().UTC()); err != nil {
		return nil, err
	}
	err = act(vm, p)
	obs, oerr := s.Observe(ctx, vm.ID)
	if oerr != nil {
		s.log.Warn("vm: could not observe the VM after an action", "vm_id", vm.ID, "error", oerr)
	}
	if err != nil {
		return nil, failure(err)
	}
	s.log.Info("vm "+task.Type, "vm_id", vm.ID, "state", obs.State)
	return ActionResult{VMID: vm.ID, State: obs.State, Detail: obs.Detail, PID: obs.PID}, nil
}

func (s *Service) runStart(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	return s.actionTask(ctx, task, models.VMRunning, func(vm models.VM, _ actionPayload) error {
		report.Progress("starting the runner", nil)
		return s.hv.Start(ctx, vm)
	})
}

func (s *Service) runStop(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	return s.actionTask(ctx, task, models.VMStopped, func(vm models.VM, p actionPayload) error {
		report.Progress("asking the guest to shut down", map[string]any{"timeout_seconds": int(p.timeout() / time.Second)})
		return s.hv.Stop(ctx, vm.ID, p.timeout())
	})
}

func (s *Service) runForceStop(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	return s.actionTask(ctx, task, models.VMStopped, func(vm models.VM, _ actionPayload) error {
		report.Progress("powering the guest off", nil)
		return s.hv.ForceStop(ctx, vm.ID)
	})
}

func (s *Service) runRestart(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	return s.actionTask(ctx, task, models.VMRunning, func(vm models.VM, p actionPayload) error {
		report.Progress("asking the guest to shut down", map[string]any{"timeout_seconds": int(p.timeout() / time.Second)})
		if err := s.hv.Stop(ctx, vm.ID, p.timeout()); err != nil {
			return err
		}
		if err := s.checkMemory(ctx, vm); err != nil {
			return err
		}
		report.Progress("starting the runner", nil)
		return s.hv.Start(ctx, vm)
	})
}

// Watch observes every VM every interval until ctx ends (ADR 0009
// decision 7b). It writes observed columns only.
func (s *Service) Watch(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.ObserveAll(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("vm: observing the VMs failed", "error", err)
			}
		}
	}
}
