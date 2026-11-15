package vm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/ids"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Disk changes after creation (ADR 0010 decision 4): add and grow while
// stopped, never shrink, and remove only with vm:delete, destroying every
// snapshot that contains the removed disk whole when destroy_snapshots
// says so.

// diskWork is a checked disk plan with the snapshots it destroys.
type diskWork struct {
	plan diskPlan
	// destroy are the snapshots that contain a removed disk, oldest
	// first.
	destroy []models.Snapshot
}

// checkDisks checks a disk plan against the host and the snapshots: the
// space new reservations need, and the snapshots a removal would destroy.
func (s *Service) checkDisks(ctx context.Context, vm models.VM, plan diskPlan, destroySnapshots bool) (diskWork, error) {
	work := diskWork{plan: plan}
	if plan.empty() {
		return work, nil
	}
	if err := s.checkDiskSpace(ctx, plan); err != nil {
		return work, err
	}
	if len(plan.Remove) == 0 {
		return work, nil
	}
	// Which snapshots contain a removed disk: ZFS says, per zvol.
	removing := map[string]bool{}
	for _, d := range plan.Remove {
		removing[d.Dataset] = true
	}
	containing := map[string]bool{}
	var foreign []string
	for _, pool := range diskPools(vm) {
		if s.host.Conf.IsDirectory(pool) {
			continue // a directory pool has no snapshots
		}
		found, err := s.host.ZFS.Snapshots(ctx, pool, vmOwner(vm.ID))
		if err != nil {
			return work, err
		}
		for _, d := range found {
			base, short, _ := strings.Cut(d.Name, "@")
			if !removing[base] {
				continue
			}
			if d.SnapshotID == "" || short != hostops.SnapshotPrefix+d.SnapshotID {
				foreign = append(foreign, d.Name)
				continue
			}
			containing[d.SnapshotID] = true
		}
	}
	if len(foreign) > 0 {
		return work, &models.Error{Code: models.CodeDiskHasSnapshots,
			Message: "the disk has snapshots that Skep did not take; an administrator removes them first",
			Details: map[string]any{"foreign_snapshots": foreign}}
	}
	if len(containing) == 0 {
		return work, nil
	}
	list, err := s.snaps.List(ctx, models.VMResource, vm.ID)
	if err != nil {
		return work, err
	}
	var refs []SnapshotRef
	var clones []models.SnapshotClone
	for _, snap := range list {
		if containing[snap.ID] {
			work.destroy = append(work.destroy, snap)
			refs = append(refs, SnapshotRef{ID: snap.ID, Name: snap.Name})
			clones = append(clones, snap.Clones...)
		}
	}
	// A snapshot with clones blocks the removal whatever the flag says.
	if len(clones) > 0 {
		return work, clonesError(clones)
	}
	if !destroySnapshots {
		return work, &models.Error{Code: models.CodeDiskHasSnapshots,
			Message: "snapshots contain the disk; removing it destroys each of them whole, on every disk of the VM, which needs destroy_snapshots",
			Details: map[string]any{"snapshots": refs}}
	}
	if len(work.destroy) != len(containing) {
		return work, fmt.Errorf("vm: ZFS has snapshots of VM %s that the database does not know", vm.ID)
	}
	return work, nil
}

// checkDiskSpace checks the reservations an update adds: new disks that
// are not thin, and the growth of disks that are not. A thin disk larger
// than its pool is refused as at create.
func (s *Service) checkDiskSpace(ctx context.Context, plan diskPlan) error {
	need := map[string]int64{}
	thinMax := map[string]int64{}
	for _, d := range plan.Add {
		if d.Thin {
			thinMax[d.Pool] = max(thinMax[d.Pool], d.SizeBytes)
		} else {
			need[d.Pool] += d.SizeBytes
		}
	}
	for _, g := range plan.Grow {
		if g.Disk.Thin {
			thinMax[g.Disk.Pool] = max(thinMax[g.Disk.Pool], g.NewBytes)
		} else {
			need[g.Disk.Pool] += g.NewBytes - g.Disk.SizeBytes
		}
	}
	pools := map[string]bool{}
	for p := range need {
		pools[p] = true
	}
	for p := range thinMax {
		pools[p] = true
	}
	for name := range pools {
		pool, err := s.host.Disks().Pool(ctx, name)
		if err != nil {
			return err
		}
		if need[name] > pool.AvailableBytes {
			return &models.Error{Code: models.CodeInsufficientSpace,
				Message: fmt.Sprintf("the pool %s has %d bytes available and the change reserves %d more; use thin disks or smaller sizes",
					name, pool.AvailableBytes, need[name]),
				Details: map[string]any{"pool": name, "available_bytes": pool.AvailableBytes, "required_bytes": need[name]}}
		}
		if thinMax[name] > pool.SizeBytes {
			return &models.Error{Code: models.CodeInsufficientSpace,
				Message: fmt.Sprintf("a thin disk of %d bytes is larger than the pool %s itself (%d bytes)", thinMax[name], name, pool.SizeBytes),
				Details: map[string]any{"pool": name, "pool_size_bytes": pool.SizeBytes, "required_bytes": thinMax[name]}}
		}
	}
	return nil
}

// applyDisks carries out a checked disk plan: the snapshots first, then
// the removals, the growth, and the new disks. Each step is recorded in
// the database as soon as it is done on the host, so a failure part way
// leaves the database describing the disks that exist.
func (s *Service) applyDisks(ctx context.Context, vm models.VM, work diskWork, result *UpdateResult, report tasks.Reporter) error {
	plan := work.plan
	if plan.empty() {
		return nil
	}
	for _, snap := range work.destroy {
		report.Progress("destroying a snapshot that contains a removed disk", map[string]any{"snapshot_id": snap.ID})
		for _, pool := range diskPools(vm) {
			if s.host.Conf.IsDirectory(pool) {
				continue
			}
			gone, err := s.host.ZFS.DestroySnapshot(ctx, pool, vmOwner(vm.ID), snap.ID)
			if errors.Is(err, models.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			result.DisksRemoved = append(result.DisksRemoved, datasetInfo(gone)...)
		}
		s.removeVars(vm.ID, snap.ID)
		if err := s.snaps.Delete(ctx, snap.ID); err != nil {
			return err
		}
		result.SnapshotsDestroyed = append(result.SnapshotsDestroyed, SnapshotRef{ID: snap.ID, Name: snap.Name})
	}
	at := s.now().UTC()
	for _, d := range plan.Remove {
		report.Progress("destroying a removed disk", map[string]any{"slot": d.Slot})
		gone, err := s.host.Disks().Destroy(ctx, hostops.VolumeSpec{Pool: d.Pool, VMID: vm.ID, DiskID: d.ID, Slot: d.Slot})
		if err != nil {
			return err
		}
		if err := s.store.RemoveDisk(ctx, vm.ID, d.ID, at); err != nil {
			return err
		}
		result.DisksRemoved = append(result.DisksRemoved, datasetInfo([]hostops.Dataset{gone})...)
	}
	for _, g := range plan.Grow {
		report.Progress("growing a disk", map[string]any{"slot": g.Disk.Slot, "size_bytes": g.NewBytes})
		grown, err := s.host.Disks().Resize(ctx, hostops.VolumeSpec{Pool: g.Disk.Pool, VMID: vm.ID, DiskID: g.Disk.ID,
			Slot: g.Disk.Slot, SizeBytes: g.NewBytes, Thin: g.Disk.Thin})
		if err != nil {
			return err
		}
		if err := s.store.ResizeDisk(ctx, vm.ID, g.Disk.ID, g.NewBytes, at); err != nil {
			return err
		}
		result.DisksGrown = append(result.DisksGrown, datasetInfo([]hostops.Dataset{grown})...)
	}
	if len(plan.Grow) > 0 {
		result.Notes = append(result.Notes,
			"the guest sees the larger disk at its next start; grow its partition and filesystem inside the guest (for example gpart recover, gpart resize, growfs on FreeBSD)")
	}
	for _, d := range plan.Add {
		report.Progress("creating a new disk", map[string]any{"slot": d.Slot, "size_bytes": d.SizeBytes})
		var err error
		if d.ID, err = ids.NewUUID(); err != nil {
			return err
		}
		if _, err := s.host.Disks().PrepareVM(ctx, d.Pool, vm.ID); err != nil {
			return err
		}
		made, err := s.host.Disks().Create(ctx, hostops.VolumeSpec{Pool: d.Pool, VMID: vm.ID, DiskID: d.ID, Slot: d.Slot,
			SizeBytes: d.SizeBytes, BlockSize: d.VolBlockSize, Thin: d.Thin})
		if err != nil {
			return err
		}
		if err := s.store.AddDisk(ctx, vm, d, s.host.Conf.DatasetRoot, at); err != nil {
			return err
		}
		result.DisksAdded = append(result.DisksAdded, datasetInfo([]hostops.Dataset{made})...)
	}
	s.refreshVolumes(ctx, vm)
	return nil
}
