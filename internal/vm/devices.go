package vm

import (
	"context"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
)

// stoppedCheck is the stopped check of a device repair: the VM has no
// runner and no instance. The caller holds the VM's task.
func (s *Service) stoppedCheck(vmID string) hostops.StoppedCheck {
	return func(ctx context.Context) error {
		obs, err := s.hv.Inspect(ctx, vmID)
		if err != nil {
			return err
		}
		if obs.Active {
			return &models.Error{Code: models.CodeVMNotStopped, Message: "the virtual machine is " + obs.State,
				Details: map[string]any{"state": obs.State}}
		}
		return nil
	}
}

// ensureDevices checks the device node of disks of a VM right after an
// operation made or replaced their zvols: create, a disk added, a clone, a
// rollback (ADR 0010 amendment z). A node that is still missing does not
// fail the operation, whose work stands: the task reports it, and the next
// start repairs the node or refuses with the reason (bhyve's
// Adapter.Start). Every caller runs inside the VM's task, with the VM
// stopped; disks are never added to a running VM.
func (s *Service) ensureDevices(ctx context.Context, vmID string, disks []models.VMDisk, report tasks.Reporter) {
	stopped := s.stoppedCheck(vmID)
	for _, d := range disks {
		if d.Bus == models.BusAHCICD || d.ID == "" {
			continue
		}
		res, err := s.host.Disks().EnsureDevice(ctx, hostops.VolumeSpec{Pool: d.Pool, VMID: vmID, DiskID: d.ID, Slot: d.Slot}, stopped)
		if err != nil {
			s.log.Warn("vm: a disk's device node is missing; the next start repairs it or says why not",
				"vm_id", vmID, "slot", d.Slot, "error", err)
			report.Progress("the device node of a disk is missing; the next start repairs it or says why not",
				map[string]any{"slot": d.Slot, "error": err.Error()})
			continue
		}
		if res.Rounds > 0 {
			report.Progress("repaired the device node of a disk", map[string]any{"slot": d.Slot, "rounds": res.Rounds})
		}
	}
}
