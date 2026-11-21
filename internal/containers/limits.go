package containers

import (
	"context"
	"errors"
	"fmt"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Jail limits (ADR 0010 decision 11) and the mlock opt-in that depends on
// them (decision 9).

// minOpenFiles keeps an openfiles limit high enough for /etc/rc to run.
const minOpenFiles = 64

// checkLimits validates a jail's limits and opt-ins against the host:
// an RCTL limit needs RACCT (rctl_unavailable), and mlock needs a
// locked-memory limit that RACCT can enforce (mlock_requires_memory_limit,
// amendment j). It runs on
// create and update, and again before every start, so a jail is never
// started without the limits it was given.
func (s *Service) checkLimits(ctx context.Context, l models.ContainerLimits, o models.ContainerOptions) error {
	info, err := s.jails.RctlInfo(ctx)
	if err != nil {
		return err
	}
	if err := validLimits(l, info.CPUs); err != nil {
		return err
	}
	if n := int64(l.LockedMemory); n > s.maxLocked {
		return &models.Error{Code: models.CodeContainerLockedMemoryAboveMax,
			Message: fmt.Sprintf("limits.locked_memory is at most %d bytes on this host: containers.max_locked_memory_bytes, the total locked memory of all running jails (by default a quarter of physical memory)", s.maxLocked),
			Details: map[string]any{"locked_memory": n, "max_locked_memory_bytes": s.maxLocked}}
	}
	if l.RCTL() && !info.Enabled {
		return &models.Error{Code: models.CodeRctlUnavailable,
			Message: "CPU, memory, process, file, and locked-memory limits need RACCT, which is disabled on this host (kern.racct.enable=1 in loader.conf, then a reboot); a disk quota works without it",
			Details: map[string]any{"racct_enabled": false}}
	}
	if o.Mlock && (l.LockedMemory == 0 || !info.Enabled) {
		return &models.Error{Code: models.CodeMlockRequiresMemoryLimit,
			Message: "mlock is accepted only together with a locked-memory limit (limits.locked_memory), because locked memory cannot be paged out"}
	}
	return nil
}

// validLimits checks the bounds of each limit.
func validLimits(l models.ContainerLimits, cpus int) error {
	switch {
	case l.Memory < 0 || l.DiskQuota < 0 || l.CPUPercent < 0 || l.MaxProcesses < 0 || l.OpenFiles < 0 || l.LockedMemory < 0:
		return models.Invalid("a limit cannot be negative; leave it out for no limit")
	case l.Memory > 0 && l.Memory < models.MinContainerMemory:
		return models.Invalid(fmt.Sprintf("limits.memory must be at least %d MiB", models.MinContainerMemory>>20))
	case l.CPUPercent > 100*cpus:
		return models.Invalid(fmt.Sprintf("limits.cpu_percent is at most %d on this host (100 per CPU, %d CPUs)", 100*cpus, cpus))
	case l.MaxProcesses > models.MaxContainerProcesses:
		return models.Invalid(fmt.Sprintf("limits.max_processes is at most %d", models.MaxContainerProcesses))
	case l.OpenFiles > 0 && l.OpenFiles < minOpenFiles:
		return models.Invalid(fmt.Sprintf("limits.open_files must be at least %d, or /etc/rc cannot run", minOpenFiles))
	case l.OpenFiles > models.MaxContainerOpenFiles:
		return models.Invalid(fmt.Sprintf("limits.open_files is at most %d", models.MaxContainerOpenFiles))
	case l.Memory > 0 && l.LockedMemory > l.Memory:
		return models.Invalid("limits.locked_memory cannot be more than limits.memory")
	case l.DiskQuota > 0 && l.DiskQuota < models.MinContainerDiskQuota:
		return models.Invalid(fmt.Sprintf("limits.disk_quota must be at least %d MiB", models.MinContainerDiskQuota>>20))
	}
	return nil
}

// checkQuotaUsage refuses a disk quota below what the jail's root already
// references with container_disk_quota_below_usage (422), before the
// update task is accepted (amendment w): ZFS refuses such a refquota, so
// the task could only fail. It reads the pool inventory, as monitoring
// does. A root missing from the inventory is left to the task, whose
// SetJailQuota reports it.
func (s *Service) checkQuotaUsage(ctx context.Context, c models.Container, quota int64) error {
	root, err := s.conf.OwnerDataset(c.Pool, jailOwner(c.ID))
	if err != nil {
		return err
	}
	list, err := s.zfs.Inventory(ctx, c.Pool)
	if err != nil {
		return err
	}
	for _, d := range list {
		if d.Name == root && !d.IsSnapshot() && d.ContainerID == c.ID {
			if quota < d.ReferencedBytes {
				return models.DiskQuotaBelowUsage(c.ID, quota, d.ReferencedBytes)
			}
			return nil
		}
	}
	return nil
}

// MaxLockedMemory is the total locked_memory of all running jails
// (amendment o, SR-04): containers.max_locked_memory_bytes when it is
// set, otherwise a quarter of physical memory. Physical memory the agent
// could not read gives 0, so no jail may lock memory.
func MaxLockedMemory(configured, physical int64) int64 {
	if configured > 0 {
		return configured
	}
	return max(physical, 0) / 4
}

// checkLockedMemoryTotal refuses a start whose locked_memory, added to
// that of the jails already running and the starts under way, would pass
// the host's total (amendment o, SR-04). It reserves nothing; the start
// itself reserves with reserveLockedMemory.
func (s *Service) checkLockedMemoryTotal(ctx context.Context, c models.Container) error {
	if c.Limits.LockedMemory <= 0 {
		return nil
	}
	s.locked.Lock()
	defer s.locked.Unlock()
	return s.lockedRoomLocked(ctx, c)
}

// reserveLockedMemory is checkLockedMemoryTotal for the start itself: on
// success the jail's locked_memory counts as a start under way until
// release is called, so two starts cannot both take the last room.
func (s *Service) reserveLockedMemory(ctx context.Context, c models.Container) (release func(), err error) {
	if c.Limits.LockedMemory <= 0 {
		return func() {}, nil
	}
	s.locked.Lock()
	defer s.locked.Unlock()
	if err := s.lockedRoomLocked(ctx, c); err != nil {
		return nil, err
	}
	s.lockedStarting[c.ID] = int64(c.Limits.LockedMemory)
	return func() {
		s.locked.Lock()
		delete(s.lockedStarting, c.ID)
		s.locked.Unlock()
	}, nil
}

// lockedRoomLocked does the check; s.locked is held. The running jails
// are what the host reports, each counted with its stored locked_memory
// (limits change only while a jail is stopped, so the stored value is the
// rule the kernel holds).
func (s *Service) lockedRoomLocked(ctx context.Context, c models.Container) error {
	running, err := s.jails.RunningJails(ctx)
	if err != nil {
		return err
	}
	var used int64
	for id := range running {
		if _, starting := s.lockedStarting[id]; id == c.ID || starting {
			continue
		}
		other, err := s.store.Get(ctx, id)
		if errors.Is(err, models.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		used += int64(other.Limits.LockedMemory)
	}
	for id, n := range s.lockedStarting {
		if id != c.ID {
			used += n
		}
	}
	want := int64(c.Limits.LockedMemory)
	if used+want > s.maxLocked {
		return &models.Error{Code: models.CodeContainerLockedMemoryExhausted,
			Message: fmt.Sprintf("the running jails may lock %d bytes and this one %d; containers.max_locked_memory_bytes allows %d in total. Stop a jail with locked memory, or lower this jail's limits.locked_memory",
				used, want, s.maxLocked),
			Details: map[string]any{"container_id": c.ID, "running_bytes": used, "required_bytes": want, "max_locked_memory_bytes": s.maxLocked}}
	}
	return nil
}

// Capabilities reports what jails can have on this host, for the system
// document.
func (s *Service) Capabilities(ctx context.Context) (models.ContainerCapabilities, error) {
	info, err := s.jails.RctlInfo(ctx)
	if err != nil {
		return models.ContainerCapabilities{}, err
	}
	return models.ContainerCapabilities{RCTL: info.Enabled, MaxCPUPercent: 100 * info.CPUs}, nil
}

// jailLimits are a container's RCTL limits as hostops takes them.
func jailLimits(l models.ContainerLimits) hostops.JailLimits {
	return hostops.JailLimits{MemoryBytes: int64(l.Memory), CPUPercent: l.CPUPercent,
		MaxProcesses: l.MaxProcesses, OpenFiles: l.OpenFiles, LockedMemoryBytes: int64(l.LockedMemory)}
}
