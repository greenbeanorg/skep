package hostops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/greenbeanorg/skep/pkg/models"
)

// Jail resource limits (ADR 0010 decision 11): RCTL rules on
// jail:skep-<id>, all with the deny action, and refquota on the jail root.
//
//   - The rules are added after jail -c, while nothing runs in the jail,
//     and read back before the /dev guard and /etc/rc; a jail whose rules
//     are not exactly the expected ones is removed before any of its code
//     runs.
//   - RCTL keeps rules on a jail name after the jail is gone, so every
//     removal (stop, a failed start, a discard) removes them, and every
//     start removes what an earlier run left before it adds its own.
//   - With RACCT disabled there are no rules to add or remove; a jail that
//     asks for a limit is refused with rctl_unavailable, never started
//     without it.

// rctlBinary is rctl(8). It takes a rule or a filter as one argument.
const rctlBinary = "/usr/bin/rctl"

// sysctlRacct is 1 when RACCT is compiled in and was enabled at boot.
const sysctlRacct = "kern.racct.enable"

// The RCTL resources Skep sets (decision 11).
const (
	rctlMemoryUse = "memoryuse"
	rctlMemLocked = "memorylocked"
	rctlPCPU      = "pcpu"
	rctlMaxProc   = "maxproc"
	rctlOpenFiles = "openfiles"
)

// JailLimits are the RCTL limits of a jail; zero means none. CPUPercent
// is RCTL's pcpu, where 100 is one CPU. LockedMemoryBytes is
// memorylocked, the limit the mlock opt-in needs (amendment j).
type JailLimits struct {
	MemoryBytes       int64
	CPUPercent        int
	MaxProcesses      int
	OpenFiles         int
	LockedMemoryBytes int64
}

// Any reports whether a limit is set.
func (l JailLimits) Any() bool {
	return l.MemoryBytes > 0 || l.CPUPercent > 0 || l.MaxProcesses > 0 || l.OpenFiles > 0 ||
		l.LockedMemoryBytes > 0
}

// RctlInfo is what the host offers for jail limits.
type RctlInfo struct {
	// Enabled is kern.racct.enable: RACCT is compiled in and was turned
	// on at boot.
	Enabled bool
	// CPUs is hw.ncpu, the bound of a pcpu limit (100 per CPU).
	CPUs int
}

// JailRctlRules are the rules of a jail's limits, in a fixed order, as
// rctl -a takes them and as rctl lists them.
func JailRctlRules(containerID string, l JailLimits) ([]string, error) {
	name, err := JailName(containerID)
	if err != nil {
		return nil, err
	}
	if l.MemoryBytes < 0 || l.CPUPercent < 0 || l.MaxProcesses < 0 || l.OpenFiles < 0 || l.LockedMemoryBytes < 0 {
		return nil, invalid("a jail limit cannot be negative")
	}
	var rules []string
	add := func(resource string, n int64) {
		if n > 0 {
			rules = append(rules, "jail:"+name+":"+resource+":deny="+strconv.FormatInt(n, 10))
		}
	}
	add(rctlMemoryUse, l.MemoryBytes)
	add(rctlPCPU, int64(l.CPUPercent))
	add(rctlMaxProc, int64(l.MaxProcesses))
	add(rctlOpenFiles, int64(l.OpenFiles))
	add(rctlMemLocked, l.LockedMemoryBytes)
	return rules, nil
}

// rctlUnavailable is the refusal of a limit on a host without RACCT.
func rctlUnavailable(containerID string) error {
	return &models.Error{Code: models.CodeRctlUnavailable,
		Message: "the jail has CPU, memory, process, file, or locked-memory limits, and RACCT is disabled on this host (kern.racct.enable=1 in loader.conf, then a reboot); Skep does not start a jail without the limits it was given",
		Details: map[string]any{"container_id": containerID}}
}

// RctlInfo implements Jails.
func (h *host) RctlInfo(context.Context) (RctlInfo, error) {
	var info RctlInfo
	on, err := h.sys.SysctlUint64(sysctlRacct)
	switch {
	case errors.Is(err, errNoSuchOID):
		// A kernel without RACCT.
	case err != nil:
		return info, fmt.Errorf("hostops: reading %s: %w", sysctlRacct, err)
	default:
		info.Enabled = on == 1
	}
	n, err := h.sys.SysctlUint64(sysctlNCPU)
	if err != nil {
		return info, fmt.Errorf("hostops: reading %s: %w", sysctlNCPU, err)
	}
	info.CPUs = int(n)
	return info, nil
}

// jailRules lists the RCTL rules on a jail's name.
func (h *host) jailRules(ctx context.Context, name string) ([]string, error) {
	out, err := h.run.Run(ctx, rctlBinary, "jail:"+name)
	if err != nil {
		return nil, err
	}
	var rules []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// rctl prints the per-subject only when it differs from the
		// subject; strip it if a release prints it anyway.
		rules = append(rules, strings.TrimSuffix(line, "/jail"))
	}
	slices.Sort(rules)
	return rules, nil
}

// clearJailRules removes every RCTL rule on a jail's name and confirms
// none is left. Without RACCT there is nothing to remove.
func (h *host) clearJailRules(ctx context.Context, containerID string) error {
	info, err := h.RctlInfo(ctx)
	if err != nil || !info.Enabled {
		return err
	}
	name, err := JailName(containerID)
	if err != nil {
		return err
	}
	rules, err := h.jailRules(ctx, name)
	if err != nil || len(rules) == 0 {
		return err
	}
	// rctl -r exits 1 when no rule matched, so it runs only when the
	// listing showed some.
	if _, err := h.run.Run(ctx, rctlBinary, "-r", "jail:"+name); err != nil {
		return err
	}
	if left, err := h.jailRules(ctx, name); err != nil {
		return err
	} else if len(left) > 0 {
		return fmt.Errorf("hostops: RCTL rules are left on %s after rctl -r: %s", name, strings.Join(left, " "))
	}
	h.log.Info("hostops: removed the RCTL rules of a jail", "event", "jail_rctl_removed", "container_id", containerID, "rules", strings.Join(rules, " "))
	return nil
}

// applyJailRules adds a jail's rules and checks that the kernel holds
// exactly those on its name.
func (h *host) applyJailRules(ctx context.Context, spec JailSpec) error {
	want, err := JailRctlRules(spec.ContainerID, spec.Limits)
	if err != nil || len(want) == 0 {
		return err
	}
	for _, rule := range want {
		if _, err := h.run.Run(ctx, rctlBinary, "-a", rule); err != nil {
			return fmt.Errorf("rctl -a %s: %w", rule, err)
		}
	}
	name, _ := JailName(spec.ContainerID)
	got, err := h.jailRules(ctx, name)
	if err != nil {
		return err
	}
	sorted := slices.Clone(want)
	slices.Sort(sorted)
	if !slices.Equal(got, sorted) {
		return fmt.Errorf("the jail's RCTL rules are %q, want %q", got, sorted)
	}
	return nil
}

// checkRctlForStart refuses a jail with limits on a host without RACCT
// and removes the rules an earlier run left on the jail's name.
func (h *host) checkRctlForStart(ctx context.Context, spec JailSpec) error {
	info, err := h.RctlInfo(ctx)
	if err != nil {
		return err
	}
	if spec.Limits.Any() && !info.Enabled {
		return rctlUnavailable(spec.ContainerID)
	}
	return h.clearJailRules(ctx, spec.ContainerID)
}

// SetJailQuota implements Jails.
func (h *host) SetJailQuota(ctx context.Context, pool, containerID string, bytes int64) error {
	if bytes < 0 {
		return invalid("a disk quota cannot be negative")
	}
	ds, err := h.conf.JailDataset(pool, containerID)
	if err != nil {
		return err
	}
	if err := h.zfsLoaded(); err != nil {
		return err
	}
	d, err := h.ownedJail(ctx, ds, containerID)
	if err != nil {
		return err
	}
	if d == nil {
		return notFound("the jail dataset " + ds)
	}
	value := "none"
	if bytes > 0 {
		value = strconv.FormatInt(bytes, 10)
	}
	if _, err := h.run.Run(ctx, zfsBinary, "set", "refquota="+value, ds); err != nil {
		if stderrContains(err, "less than current used") || stderrContains(err, "size is less than") {
			return models.Invalid(fmt.Sprintf("the disk quota is below what the jail's root already uses (%d bytes)", d.ReferencedBytes))
		}
		return err
	}
	return nil
}
