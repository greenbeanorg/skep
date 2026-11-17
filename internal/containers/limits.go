package containers

import (
	"context"
	"fmt"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Jail limits (ADR 0010 decision 11) and the mlock opt-in that depends on
// them (decision 9).

// minOpenFiles keeps an openfiles limit high enough for /etc/rc to run.
const minOpenFiles = 64

// checkLimits validates a jail's limits and opt-ins against the host:
// an RCTL limit needs RACCT (rctl_unavailable), and mlock needs a memory
// limit that RACCT can enforce (mlock_requires_memory_limit). It runs on
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
	if l.RCTL() && !info.Enabled {
		return &models.Error{Code: models.CodeRctlUnavailable,
			Message: "CPU, memory, process, and file limits need RACCT, which is disabled on this host (kern.racct.enable=1 in loader.conf, then a reboot); a disk quota works without it",
			Details: map[string]any{"racct_enabled": false}}
	}
	if o.Mlock && (l.Memory == 0 || !info.Enabled) {
		return &models.Error{Code: models.CodeMlockRequiresMemoryLimit,
			Message: "mlock is accepted only together with a memory limit (limits.memory), because locked memory cannot be paged out"}
	}
	return nil
}

// validLimits checks the bounds of each limit.
func validLimits(l models.ContainerLimits, cpus int) error {
	switch {
	case l.Memory < 0 || l.DiskQuota < 0 || l.CPUPercent < 0 || l.MaxProcesses < 0 || l.OpenFiles < 0:
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
	case l.DiskQuota > 0 && l.DiskQuota < models.MinContainerDiskQuota:
		return models.Invalid(fmt.Sprintf("limits.disk_quota must be at least %d MiB", models.MinContainerDiskQuota>>20))
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
		MaxProcesses: l.MaxProcesses, OpenFiles: l.OpenFiles}
}
