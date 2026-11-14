// Package bhyve is the bhyve adapter of the hypervisor interface: UEFI
// guests with zvol-backed disks (ADR 0009). Everything it does on the host
// goes through internal/hostops.
package bhyve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/hypervisor"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Options configures the adapter.
type Options struct {
	Host hostops.Ops
	// HostMemoryReserveBytes is the memory left to the host.
	HostMemoryReserveBytes int64
	// MaxVCPUsPerVM bounds a VM's CPU count; 0 means the lower of the
	// host's logical CPUs and hw.vmm.maxcpu.
	MaxVCPUsPerVM int
	Logger        *slog.Logger
}

// Adapter implements hypervisor.Hypervisor with bhyve.
type Adapter struct {
	host    hostops.Ops
	reserve int64
	maxCPU  int
	log     *slog.Logger
}

var _ hypervisor.Hypervisor = (*Adapter)(nil)

// New returns the bhyve adapter.
func New(opts Options) *Adapter {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Adapter{host: opts.Host, reserve: opts.HostMemoryReserveBytes, maxCPU: opts.MaxVCPUsPerVM, log: log}
}

// Engine implements hypervisor.Hypervisor.
func (a *Adapter) Engine() string { return models.EngineBhyve }

// Capabilities implements hypervisor.Hypervisor.
func (a *Adapter) Capabilities(ctx context.Context) models.EngineCapabilities {
	info := a.host.VMM.Capabilities(ctx)
	caps := models.EngineCapabilities{
		Simulated:       info.Simulated,
		VMMLoaded:       info.VMMLoaded,
		FirmwareFound:   info.FirmwareCode && info.FirmwareVars,
		NmdmAvailable:   info.NmdmAvailable,
		HostMemoryBytes: info.HostMemoryBytes,
		Pools:           append([]string{}, a.host.Conf.Pools...),
		Bridges:         append([]string{}, a.host.Conf.Bridges...),
	}
	if budget := info.HostMemoryBytes - a.reserve; budget > 0 {
		caps.GuestMemoryBytes = budget
	}
	// The lower of the host's logical CPUs and what vmm allows, and of
	// the configured bound when there is one.
	caps.MaxVCPUsPerVM = info.HostCPUs
	if info.MaxCPU > 0 && info.MaxCPU < caps.MaxVCPUsPerVM {
		caps.MaxVCPUsPerVM = info.MaxCPU
	}
	if a.maxCPU > 0 && a.maxCPU < caps.MaxVCPUsPerVM {
		caps.MaxVCPUsPerVM = a.maxCPU
	}

	var missing []string
	if !info.BhyvePresent {
		missing = append(missing, "the bhyve binary is missing")
	}
	if !info.VMMLoaded {
		missing = append(missing, "the vmm kernel module is not loaded (an administrator sets vmm_load=\"YES\" in /boot/loader.conf; Skep loads nothing)")
	}
	if !caps.FirmwareFound {
		missing = append(missing, "the UEFI firmware ("+hostops.FirmwareCodeFile+" and "+hostops.FirmwareVarsFile+
			") is not in "+a.host.Conf.FirmwareDir+" (install the edk2-bhyve package)")
	}
	if !info.NmdmAvailable {
		missing = append(missing, "the nmdm kernel module is not loaded (an administrator sets nmdm_load=\"YES\" in /boot/loader.conf)")
	}
	switch {
	case len(missing) > 0:
		caps.Reason = strings.Join(missing, "; ")
	case info.Simulated:
		caps.Available = true
		caps.Reason = "simulated: this is a development machine and no VM runs"
	default:
		caps.Available = true
		caps.Reason = "vmm is loaded, the UEFI firmware is installed, and nmdm is available"
	}
	return caps
}

// unavailable returns the bhyve_unavailable error when the engine cannot
// run a VM, with the reason Capabilities gives.
func (a *Adapter) unavailable(ctx context.Context) error {
	caps := a.Capabilities(ctx)
	if caps.Available {
		return nil
	}
	return &models.Error{Code: models.CodeBhyveUnavailable, Message: "bhyve cannot run a VM on this host: " + caps.Reason}
}

// Validate implements hypervisor.Hypervisor.
func (a *Adapter) Validate(ctx context.Context, vm models.VM) ([]string, error) {
	if vm.Engine != models.EngineBhyve {
		return nil, &models.Error{Code: models.CodeEngineUnsupported,
			Message: "the engine " + vm.Engine + " is not supported; only bhyve is"}
	}
	if vm.Firmware != models.FirmwareUEFI {
		return nil, &models.Error{Code: models.CodeFirmwareUnsupported,
			Message: "bhyve boots with UEFI firmware only; the firmware " + vm.Firmware + " is not supported"}
	}
	if vm.Config.Console.VNC.Enabled {
		return nil, &models.Error{Code: models.CodeNotImplemented,
			Message: "the VNC console is not implemented; a VM has a serial console"}
	}
	caps := a.Capabilities(ctx)
	if caps.MaxVCPUsPerVM > 0 && vm.CPUCount > caps.MaxVCPUsPerVM {
		return nil, models.Invalid(fmt.Sprintf("cpu_count must be at most %d on this host", caps.MaxVCPUsPerVM))
	}
	if caps.GuestMemoryBytes > 0 && vm.MemoryBytes > caps.GuestMemoryBytes {
		return nil, models.Invalid(fmt.Sprintf(
			"memory must be at most %d bytes on this host: physical memory less the configured reserve", caps.GuestMemoryBytes))
	}
	for _, nic := range vm.NICs {
		if err := a.host.Conf.CheckBridge(nic.Bridge); err != nil {
			return nil, err
		}
	}
	for _, d := range vm.Disks {
		if d.Bus == models.BusAHCICD {
			continue
		}
		if err := a.host.Conf.CheckPool(d.Pool); err != nil {
			return nil, err
		}
	}
	if vm.Provisioned {
		return nil, nil
	}
	return a.checkSpace(ctx, vm)
}

// checkSpace checks the disks of a VM that does not exist yet against
// their pools (ADR 0009 decision 3c): a full reservation must fit in what
// is available; a thin disk may exceed that, with a warning, but not the
// pool itself.
func (a *Adapter) checkSpace(ctx context.Context, vm models.VM) ([]string, error) {
	full, thin := map[string]int64{}, map[string]int64{}
	var order []string
	for _, d := range vm.Disks {
		if d.Bus == models.BusAHCICD {
			continue
		}
		if _, seen := full[d.Pool]; !seen {
			full[d.Pool], thin[d.Pool] = 0, 0
			order = append(order, d.Pool)
		}
		if d.Thin {
			thin[d.Pool] += d.SizeBytes
		} else {
			full[d.Pool] += d.SizeBytes
		}
	}
	var warnings []string
	for _, name := range order {
		pool, err := a.host.ZFS.Pool(ctx, name)
		if err != nil {
			return nil, err
		}
		if full[name] > pool.AvailableBytes {
			return nil, &models.Error{
				Code: models.CodeInsufficientSpace,
				Message: fmt.Sprintf("the pool %s has %d bytes available and the disks reserve %d; use a smaller disk, or thin: true to create it without a reservation",
					name, pool.AvailableBytes, full[name]),
				Details: map[string]any{"pool": name, "available_bytes": pool.AvailableBytes, "required_bytes": full[name]},
			}
		}
		for _, d := range vm.Disks {
			if d.Bus != models.BusAHCICD && d.Pool == name && d.Thin && d.SizeBytes > pool.SizeBytes {
				return nil, &models.Error{
					Code:    models.CodeInsufficientSpace,
					Message: fmt.Sprintf("a thin disk of %d bytes is larger than the pool %s itself (%d bytes)", d.SizeBytes, name, pool.SizeBytes),
					Details: map[string]any{"pool": name, "pool_size_bytes": pool.SizeBytes, "required_bytes": d.SizeBytes},
				}
			}
		}
		if thin[name] > 0 && full[name]+thin[name] > pool.AvailableBytes {
			warnings = append(warnings, fmt.Sprintf(
				"the thin disks on pool %s can grow to %d bytes, but only %d bytes are available: the guest gets I/O errors if the pool fills",
				name, thin[name], pool.AvailableBytes-full[name]))
		}
	}
	return warnings, nil
}

// pools returns the pools the disks of a VM live in, in slot order.
func pools(vm models.VM) []string {
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

// Create implements hypervisor.Hypervisor: one dataset per VM and pool,
// one zvol per disk, and the persistent directory with the VM's copy of
// the UEFI variable store.
func (a *Adapter) Create(ctx context.Context, vm models.VM) (hypervisor.Provisioned, error) {
	var made hypervisor.Provisioned
	for _, pool := range pools(vm) {
		if err := ctx.Err(); err != nil {
			return made, err
		}
		ds, err := a.host.ZFS.CreateVMDataset(ctx, pool, vm.ID)
		if err != nil {
			return made, err
		}
		made.Datasets = append(made.Datasets, ds)
	}
	for _, d := range vm.Disks {
		if d.Bus == models.BusAHCICD {
			continue
		}
		if err := ctx.Err(); err != nil {
			return made, err
		}
		ds, err := a.host.ZFS.CreateVolume(ctx, hostops.VolumeSpec{
			Pool: d.Pool, VMID: vm.ID, DiskID: d.ID, Slot: d.Slot,
			SizeBytes: d.SizeBytes, BlockSize: d.VolBlockSize, Thin: d.Thin,
		})
		if err != nil {
			return made, err
		}
		made.Datasets = append(made.Datasets, ds)
	}
	if err := ctx.Err(); err != nil {
		return made, err
	}
	if err := a.host.Files.PrepareVM(ctx, vm.ID); err != nil {
		return made, err
	}
	return made, nil
}

// ErrActive is returned by Delete for a VM that still has a runner or an
// instance on the host.
var ErrActive = &models.Error{
	Code:    models.CodeVMNotStopped,
	Message: "the virtual machine is not stopped",
}

// Delete implements hypervisor.Hypervisor.
func (a *Adapter) Delete(ctx context.Context, vm models.VM) (hypervisor.Removed, error) {
	var gone hypervisor.Removed
	obs, err := a.Inspect(ctx, vm.ID)
	if err != nil {
		return gone, err
	}
	if obs.Active {
		return gone, ErrActive
	}
	for _, pool := range pools(vm) {
		// A pool taken off the allow-list after the VM was made is no
		// longer Skep's to touch: say so instead of destroying.
		destroyed, err := a.host.ZFS.DestroyVM(ctx, pool, vm.ID)
		if err != nil {
			var me *models.Error
			if errors.As(err, &me) {
				return gone, err
			}
			return gone, fmt.Errorf("bhyve: destroy the datasets of VM %s in pool %s: %w", vm.ID, pool, err)
		}
		gone.Datasets = append(gone.Datasets, destroyed...)
	}
	if err := a.host.Files.RemoveVM(ctx, vm.ID); err != nil {
		return gone, err
	}
	return gone, nil
}

// Inspect implements hypervisor.Hypervisor.
func (a *Adapter) Inspect(_ context.Context, vmID string) (hypervisor.Observation, error) {
	if !hostops.ValidID(vmID) {
		return hypervisor.Observation{}, models.Invalid("invalid VM ID")
	}
	obs := hypervisor.Observation{VMID: vmID, State: models.VMStopped}
	if a.host.VMM.InstanceExists(vmID) {
		obs.State, obs.Active = models.VMFailed, true
		obs.Detail = "a vmm instance exists without a runner; force-stop the VM to destroy it"
	}
	return obs, nil
}

// List implements hypervisor.Hypervisor.
func (a *Adapter) List(context.Context) ([]hypervisor.Observation, error) {
	return []hypervisor.Observation{}, nil
}
