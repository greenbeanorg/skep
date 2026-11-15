package hostops

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// psBinary reads a process's accounting from kern.proc. It is used
// instead of decoding struct kinfo_proc by hand: golang.org/x/sys has no
// FreeBSD binding for it, and its layout is not a stable interface.
const psBinary = "/bin/ps"

// BhyveUsage implements Procs.
func (h *host) BhyveUsage(ctx context.Context, vmID string, pid int) (ProcessUsage, error) {
	instance, err := Instance(vmID)
	if err != nil {
		return ProcessUsage{}, err
	}
	if pid <= 1 {
		return ProcessUsage{}, fmt.Errorf("process %d: %w", pid, models.ErrNotFound)
	}
	// The process must be this VM's bhyve: a PID observed earlier may
	// have been recycled.
	args, err := h.sys.ProcessArgs(pid)
	if err != nil || !isBhyveOf(args, instance) {
		return ProcessUsage{}, fmt.Errorf("bhyve of %s (process %d): %w", vmID, pid, models.ErrNotFound)
	}
	out, err := h.run.Run(ctx, psBinary, "-o", "rss=,time=,comm=", "-p", strconv.Itoa(pid))
	if err != nil {
		if ce, ok := err.(*CommandError); ok && ce.ExitCode == 1 {
			// ps exits 1 when the process is gone.
			return ProcessUsage{}, fmt.Errorf("process %d: %w", pid, models.ErrNotFound)
		}
		return ProcessUsage{}, err
	}
	return parsePS(out)
}

// isBhyveOf reports whether a process's arguments are those of bhyve
// running the vmm instance: bhyve's last argument is the instance name,
// and bhyve renames itself to "bhyve: <instance>" with setproctitle(3),
// which is what kern.proc.args then returns.
func isBhyveOf(args []string, instance string) bool {
	if len(args) == 0 {
		return false
	}
	if len(args) == 1 {
		title := strings.TrimSpace(args[0])
		return title == "bhyve: "+instance
	}
	return (args[0] == bhyveBinary || args[0] == "bhyve") && args[len(args)-1] == instance
}

// parsePS reads one line of "ps -o rss=,time=,comm=": the resident set
// in KiB, the CPU time, and the command name, which must be bhyve.
func parsePS(out []byte) (ProcessUsage, error) {
	f := strings.Fields(firstLine(out))
	if len(f) != 3 {
		return ProcessUsage{}, fmt.Errorf("ps: unexpected output %q", firstLine(out))
	}
	if f[2] != "bhyve" {
		return ProcessUsage{}, fmt.Errorf("ps: process is %q, not bhyve: %w", f[2], models.ErrNotFound)
	}
	kib, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil || kib < 0 {
		return ProcessUsage{}, fmt.Errorf("ps: unexpected resident size %q", f[0])
	}
	cpu, err := parseCPUTime(f[1])
	if err != nil {
		return ProcessUsage{}, err
	}
	return ProcessUsage{CPUTime: cpu, ResidentBytes: kib * 1024}, nil
}

// parseCPUTime reads ps(1)'s CPU time: [[days-]hours:]minutes:seconds,
// the seconds with a decimal fraction (a point or, in some locales, a
// comma; the command runs with LC_ALL=C).
func parseCPUTime(s string) (time.Duration, error) {
	bad := func() (time.Duration, error) { return 0, fmt.Errorf("ps: unexpected CPU time %q", s) }
	var days int64
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.ParseInt(d, 10, 64)
		if err != nil || n < 0 {
			return bad()
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return bad()
	}
	secs, err := strconv.ParseFloat(strings.Replace(parts[len(parts)-1], ",", ".", 1), 64)
	if err != nil || secs < 0 || secs >= 60 {
		return bad()
	}
	total := time.Duration(secs * float64(time.Second))
	units := []time.Duration{time.Minute, time.Hour}
	for i := len(parts) - 2; i >= 0; i-- {
		n, err := strconv.ParseInt(parts[i], 10, 64)
		if err != nil || n < 0 {
			return bad()
		}
		total += time.Duration(n) * units[len(parts)-2-i]
	}
	return total + time.Duration(days)*24*time.Hour, nil
}
