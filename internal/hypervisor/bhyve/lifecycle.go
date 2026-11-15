package bhyve

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/hypervisor"
	"github.com/greenbeanorg/skep/pkg/models"
)

// How long the lifecycle methods wait for the runner. The task time
// limits of ADR 0009 decision 9 bound them from above.
const (
	startWait = 90 * time.Second
	forceWait = 30 * time.Second
)

// pollInterval is how often a lifecycle method looks at the run state.
var pollInterval = 100 * time.Millisecond

// unavailable returns the bhyve_unavailable error when the engine cannot
// run a VM, with the reason Capabilities gives.
func (a *Adapter) unavailable(ctx context.Context) error {
	caps := a.Capabilities(ctx)
	if caps.Available {
		return nil
	}
	return &models.Error{Code: models.CodeBhyveUnavailable, Message: "bhyve cannot run a VM on this host: " + caps.Reason}
}

// spec builds what the runner needs: every path derived here, from IDs
// and the confinement.
func (a *Adapter) spec(ctx context.Context, vm models.VM) (Spec, error) {
	s := Spec{
		Conf: a.host.Conf, VMID: vm.ID, Name: vm.Name, CPUs: vm.CPUCount, Topology: vm.Config.CPU,
		MemoryBytes: vm.MemoryBytes, Disks: []SpecDisk{}, NICs: []SpecNIC{},
	}
	for _, d := range vm.Disks {
		sd := SpecDisk{Slot: d.Slot, Bus: d.Bus, ReadOnly: d.ReadOnly}
		if d.Bus == models.BusAHCICD {
			path, err := a.host.Files.ResolveMedia(ctx, d.Media)
			if err != nil {
				return Spec{}, err
			}
			sd.Path = path
		} else {
			path, err := a.host.Conf.DiskPath(d.Pool, vm.ID, d.Slot)
			if err != nil {
				return Spec{}, err
			}
			sd.Path = path
		}
		s.Disks = append(s.Disks, sd)
	}
	for _, n := range vm.NICs {
		b, err := a.host.Net.Bridge(ctx, n.Bridge)
		if err != nil {
			return Spec{}, err
		}
		if err := hostops.CheckBridgeUsable(a.host.Conf, b); err != nil {
			return Spec{}, err
		}
		s.NICs = append(s.NICs, SpecNIC{Slot: n.Slot, Model: n.Model, MAC: n.MACAddress, Bridge: n.Bridge})
	}
	return s, nil
}

// Start implements hypervisor.Hypervisor.
func (a *Adapter) Start(ctx context.Context, vm models.VM) error {
	if err := a.unavailable(ctx); err != nil {
		return err
	}
	if _, err := a.Validate(ctx, vm); err != nil {
		return err
	}
	obs, err := a.Inspect(ctx, vm.ID)
	if err != nil {
		return err
	}
	if obs.Active {
		return &models.Error{Code: models.CodeVMNotStopped,
			Message: "the virtual machine is " + obs.State + withDetail(obs.Detail),
			Details: map[string]any{"state": obs.State}}
	}
	spec, err := a.spec(ctx, vm)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	if err := a.host.Files.PrepareRun(ctx, vm.ID); err != nil {
		return err
	}
	if err := a.host.Files.WriteRunFile(vm.ID, hostops.RunSpecFile, raw); err != nil {
		return err
	}
	// The previous run's outcome is not this run's.
	if err := a.host.Files.RemoveRunFile(vm.ID, hostops.RunStateFile); err != nil {
		return err
	}
	pid, err := a.host.Procs.StartRunner(ctx, vm.ID)
	if err != nil {
		return err
	}
	a.log.Info("bhyve: runner started", "vm_id", vm.ID, "runner_pid", pid)

	deadline := time.Now().Add(startWait)
	for {
		obs, err := a.Inspect(ctx, vm.ID)
		if err != nil {
			return err
		}
		switch {
		case obs.State == models.VMRunning:
			return nil
		case !obs.Active:
			return &models.Error{Code: models.CodeVMStartFailed,
				Message: "the virtual machine did not start" + withDetail(obs.Detail),
				Details: map[string]any{"state": obs.State}}
		case time.Now().After(deadline):
			return &models.Error{Code: models.CodeVMStartFailed,
				Message: fmt.Sprintf("the virtual machine did not report running within %s; it is %s", startWait, obs.State),
				Details: map[string]any{"state": obs.State}}
		}
		if err := sleep(ctx, pollInterval); err != nil {
			return err
		}
	}
}

// withDetail formats an optional detail.
func withDetail(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func notRunning(obs hypervisor.Observation) error {
	return &models.Error{Code: models.CodeVMNotRunning,
		Message: "the virtual machine is " + obs.State + withDetail(obs.Detail),
		Details: map[string]any{"state": obs.State}}
}

// Stop implements hypervisor.Hypervisor.
func (a *Adapter) Stop(ctx context.Context, vmID string, timeout time.Duration) error {
	obs, err := a.Inspect(ctx, vmID)
	if err != nil {
		return err
	}
	if !obs.Active {
		return nil // already stopped
	}
	if _, ok := a.host.Procs.Runner(vmID); !ok {
		// An instance without a runner has no one to ask.
		return notRunning(obs)
	}
	if err := a.host.Procs.SignalRunner(vmID, hostops.StopGraceful); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := sleep(ctx, pollInterval); err != nil {
			return err
		}
		obs, err := a.Inspect(ctx, vmID)
		if err != nil {
			return err
		}
		if !obs.Active {
			return nil
		}
		if time.Now().After(deadline) {
			return &models.Error{Code: models.CodeStopTimeout,
				Message: fmt.Sprintf("the guest did not shut down within %s; it is still running. Stop it again, or force-stop it", timeout),
				Details: map[string]any{"timeout_seconds": int(timeout / time.Second)}}
		}
	}
}

// ForceStop implements hypervisor.Hypervisor.
func (a *Adapter) ForceStop(ctx context.Context, vmID string) error {
	if !hostops.ValidID(vmID) {
		return models.Invalid("invalid VM ID")
	}
	if _, ok := a.host.Procs.Runner(vmID); ok {
		if err := a.host.Procs.SignalRunner(vmID, hostops.StopForce); err != nil {
			return err
		}
		deadline := time.Now().Add(forceWait)
		for {
			if _, ok := a.host.Procs.Runner(vmID); !ok {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("bhyve: the runner of VM %s did not end within %s of a force stop", vmID, forceWait)
			}
			if err := sleep(ctx, pollInterval); err != nil {
				return err
			}
		}
	}
	// Whatever is left without a runner: the instance, and taps.
	if err := a.host.VMM.DestroyInstance(ctx, vmID); err != nil {
		return err
	}
	taps, err := a.host.Net.VMTaps(ctx, vmID)
	if err != nil {
		return err
	}
	for _, t := range taps {
		if err := a.host.Net.DestroyTap(ctx, vmID, t); err != nil {
			return err
		}
	}
	// A runner that died left a record of a run that never ended. The
	// host is clean now, so the record is removed, not kept as a failure.
	if st, ok, _ := hostops.ReadRunState(a.host.Files, vmID); ok && !st.Final {
		for _, name := range []string{hostops.RunStateFile, hostops.RunnerPIDFile} {
			if err := a.host.Files.RemoveRunFile(vmID, name); err != nil {
				return err
			}
		}
	}
	return nil
}

// Restart implements hypervisor.Hypervisor.
func (a *Adapter) Restart(ctx context.Context, vm models.VM, timeout time.Duration) error {
	if err := a.Stop(ctx, vm.ID, timeout); err != nil {
		return err
	}
	return a.Start(ctx, vm)
}

// Inspect implements hypervisor.Hypervisor: the run state the runner
// wrote, checked against whether the runner and the instance exist.
func (a *Adapter) Inspect(_ context.Context, vmID string) (hypervisor.Observation, error) {
	if !hostops.ValidID(vmID) {
		return hypervisor.Observation{}, models.Invalid("invalid VM ID")
	}
	obs := hypervisor.Observation{VMID: vmID, State: models.VMStopped}
	st, haveState, err := hostops.ReadRunState(a.host.Files, vmID)
	if err != nil {
		a.log.Warn("bhyve: unreadable run state", "vm_id", vmID, "error", err)
	}
	_, alive := a.host.Procs.Runner(vmID)
	instance := a.host.VMM.InstanceExists(vmID)
	switch {
	case alive:
		obs.Active = true
		obs.State = models.VMStarting
		if haveState && !st.Final {
			obs.State, obs.Detail, obs.PID, obs.Taps = st.State, st.Detail, st.BhyvePID, st.Taps
		}
	case instance:
		obs.State, obs.Active = models.VMFailed, true
		obs.Detail = "a vmm instance exists without a runner; force-stop the VM to destroy it"
	case haveState && !st.Final:
		obs.State = models.VMFailed
		obs.Detail = "the runner ended without recording how the run ended"
	case haveState:
		obs.State, obs.Detail = st.State, st.Detail
	}
	return obs, nil
}

// List implements hypervisor.Hypervisor: every VM with a runtime
// directory on the host.
func (a *Adapter) List(ctx context.Context) ([]hypervisor.Observation, error) {
	ids, err := a.host.Files.RunVMs()
	if err != nil {
		return nil, err
	}
	out := make([]hypervisor.Observation, 0, len(ids))
	for _, id := range ids {
		obs, err := a.Inspect(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, obs)
	}
	return out, nil
}
