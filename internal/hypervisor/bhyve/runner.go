package bhyve

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Exit statuses of bhyve(8) on FreeBSD 15.1.
const (
	exitRebooted    = 0
	exitPoweredOff  = 1
	exitHalted      = 2
	exitTripleFault = 3
	exitError       = 4
	exitSuspended   = 5
)

// forceKillDelay is how long a force stop waits after the power-off
// before it kills bhyve.
const forceKillDelay = 5 * time.Second

// maxRebootsPerMinute stops a guest that reboots in a loop.
const maxRebootsPerMinute = 10

// runner runs one VM for its whole run: the per-VM process of ADR 0009
// decision 7a. It owns the taps, the bhyve reboot loop, the instance's
// teardown, and the VM's run state. It never touches the database.
type runner struct {
	ops     hostops.Ops
	spec    Spec
	pid     int
	signals <-chan hostops.RunnerSignal
	log     *slog.Logger
	now     func() time.Time

	st    hostops.RunState
	force bool
}

// RunVM is the runner: it reads the spec the agent wrote, runs the VM
// until its run ends, cleans up, and records the outcome. signals carries
// the agent's requests. A cancelled ctx means the runner itself is dying
// (the simulated kill -9 of the development fake): it returns at once
// and cleans up nothing, as a killed process would.
func RunVM(ctx context.Context, ops hostops.Ops, vmID string, pid int, signals <-chan hostops.RunnerSignal, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	raw, err := ops.Files.ReadRunFile(vmID, hostops.RunSpecFile)
	if err != nil {
		return fmt.Errorf("runner: read the spec of VM %s: %w", vmID, err)
	}
	var spec Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return fmt.Errorf("runner: the spec of VM %s is not valid: %w", vmID, err)
	}
	if spec.VMID != vmID {
		return fmt.Errorf("runner: the spec names VM %q, not %s", spec.VMID, vmID)
	}
	r := &runner{ops: ops, spec: spec, pid: pid, signals: signals, log: log.With("vm_id", vmID), now: time.Now}
	return r.run(ctx)
}

func (r *runner) save() {
	if err := hostops.WriteRunState(r.ops.Files, r.spec.VMID, r.st); err != nil {
		r.log.Error("runner: could not write the run state", "error", err)
	}
}

func (r *runner) set(state, detail string) {
	r.st.State, r.st.Detail, r.st.Since = state, detail, r.now().UTC()
	r.save()
}

func (r *runner) run(ctx context.Context) error {
	id := r.spec.VMID
	r.st = hostops.RunState{RunnerPID: r.pid, Taps: map[int]string{}}
	r.set(models.VMStarting, "")
	_ = r.ops.Files.WriteRunFile(id, hostops.RunnerPIDFile, []byte(strconv.Itoa(r.pid)+"\n"))

	state, detail := r.boot(ctx)
	if ctx.Err() != nil {
		return nil // killed: a killed process cleans up nothing
	}
	r.teardown()
	r.st.Final, r.st.BhyvePID, r.st.Taps = true, 0, nil
	r.set(state, detail)
	_ = r.ops.Files.RemoveRunFile(id, hostops.RunnerPIDFile)
	r.log.Info("runner: the run ended", "state", state, "detail", detail, "boots", r.st.Boots)
	return nil
}

// boot prepares the host and runs bhyve until the run ends. It returns
// the final state and its detail.
func (r *runner) boot(ctx context.Context) (string, string) {
	id := r.spec.VMID
	// Left over from a run whose runner died: nothing of it is reused.
	if taps, err := r.ops.Net.VMTaps(ctx, id); err == nil {
		for _, t := range taps {
			if err := r.ops.Net.DestroyTap(ctx, id, t); err != nil {
				return models.VMFailed, "a tap left by an earlier run could not be destroyed: " + err.Error()
			}
		}
	}
	if r.ops.VMM.InstanceExists(id) {
		return models.VMFailed, "the vmm instance already exists; force-stop the VM to destroy it"
	}
	for _, n := range r.spec.NICs {
		tap, err := r.ops.Net.CreateTap(ctx, id, n.Slot)
		if err != nil {
			return models.VMFailed, "create a tap: " + err.Error()
		}
		r.st.Taps[n.Slot] = tap
		r.save()
		if err := r.ops.Net.AddBridgeMember(ctx, n.Bridge, tap); err != nil {
			return models.VMFailed, err.Error()
		}
	}
	if err := r.ops.Files.PrepareVM(ctx, id); err != nil {
		return models.VMFailed, err.Error()
	}
	vars, err := r.ops.Files.StateFilePath(id, hostops.VMVarsFile)
	if err != nil {
		return models.VMFailed, err.Error()
	}
	conf, err := Render(r.spec, r.st.Taps, filepath.Join(r.spec.Conf.FirmwareDir, hostops.FirmwareCodeFile), vars)
	if err != nil {
		return models.VMFailed, err.Error()
	}
	if err := r.ops.Files.WriteStateFile(id, hostops.BhyveConfigFile, []byte(conf)); err != nil {
		return models.VMFailed, err.Error()
	}

	var reboots []time.Time
	for {
		if r.st.StopRequested {
			return models.VMStopped, ""
		}
		state, detail, again := r.once(ctx)
		if ctx.Err() != nil || !again {
			return state, detail
		}
		// The guest rebooted: bhyve starts again with the same
		// configuration, on a fresh instance.
		if err := r.ops.VMM.DestroyInstance(ctx, id); err != nil {
			return models.VMFailed, "destroy the instance after a reboot: " + err.Error()
		}
		now := r.now()
		reboots = append(reboots, now)
		for len(reboots) > 0 && now.Sub(reboots[0]) > time.Minute {
			reboots = reboots[1:]
		}
		if len(reboots) > maxRebootsPerMinute {
			return models.VMFailed, fmt.Sprintf("the guest rebooted more than %d times in a minute", maxRebootsPerMinute)
		}
	}
}

// once runs bhyve one time. again is true when the guest rebooted and no
// stop was asked for.
func (r *runner) once(ctx context.Context) (state, detail string, again bool) {
	id := r.spec.VMID
	p, err := r.ops.VMM.StartBhyve(ctx, id)
	if err != nil {
		return models.VMFailed, "start bhyve: " + err.Error(), false
	}
	r.st.Boots++
	r.st.BhyvePID, r.st.ExitStatus = p.PID(), nil
	if r.st.StopRequested {
		r.set(models.VMStopping, "")
	} else {
		r.set(models.VMRunning, "")
	}

	exited := make(chan hostops.ExitStatus, 1)
	go func() { exited <- p.Wait() }()
	var kill <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return "", "", false
		case sig := <-r.signals:
			switch sig {
			case hostops.StopGraceful:
				if r.st.StopRequested {
					continue
				}
				r.st.StopRequested = true
				r.set(models.VMStopping, "ACPI power-off requested")
				// bhyve delivers SIGTERM to the guest as the ACPI power
				// button.
				if err := p.Signal(syscall.SIGTERM); err != nil {
					r.log.Warn("runner: could not signal bhyve", "error", err)
				}
			case hostops.StopForce:
				if r.force {
					continue
				}
				r.st.StopRequested, r.force = true, true
				r.set(models.VMStopping, "force stop requested")
				if err := r.ops.VMM.ForcePoweroff(context.Background(), id); err != nil {
					r.log.Warn("runner: force power-off failed; bhyve will be killed", "error", err)
				}
				kill = time.After(forceKillDelay)
			}
		case <-kill:
			_ = p.Signal(syscall.SIGKILL)
		case st := <-exited:
			code := st.Code
			r.st.ExitStatus, r.st.BhyvePID = &code, 0
			state, detail, again := r.outcome(st)
			if !again {
				return state, detail, false
			}
			r.save()
			return state, detail, true
		}
	}
}

// outcome maps how bhyve ended to the VM's state (ADR 0009 decision 7a).
func (r *runner) outcome(st hostops.ExitStatus) (state, detail string, again bool) {
	if r.st.StopRequested {
		if r.force {
			return models.VMStopped, "force-stopped", false
		}
		return models.VMStopped, "", false
	}
	if st.Signal != "" {
		return models.VMFailed, "bhyve was killed by " + st.Signal, false
	}
	switch st.Code {
	case exitRebooted:
		return models.VMRunning, "", true
	case exitPoweredOff:
		return models.VMStopped, "the guest powered off", false
	case exitHalted:
		return models.VMStopped, "the guest halted", false
	case exitTripleFault:
		return models.VMFailed, "the guest triple-faulted (bhyve exit status 3)", false
	case exitError:
		return models.VMFailed, "bhyve exited with an error (status 4)" + r.logHint(), false
	case exitSuspended:
		return models.VMFailed, "the guest was suspended (bhyve exit status 5)", false
	}
	if st.Err != nil {
		return models.VMFailed, "bhyve could not be waited for: " + st.Err.Error(), false
	}
	return models.VMFailed, fmt.Sprintf("bhyve exited with status %d", st.Code) + r.logHint(), false
}

// logHint is the last line bhyve wrote, for a failure's detail.
func (r *runner) logHint() string {
	tail := strings.TrimSpace(r.ops.Files.LogTail(r.spec.VMID, 1024))
	if tail == "" {
		return ""
	}
	if i := strings.LastIndexByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}
	return ": " + strings.TrimSpace(tail)
}

// teardown destroys the instance and the taps of the run. It uses its own
// context: whatever ended the run must not stop the cleanup.
func (r *runner) teardown() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	id := r.spec.VMID
	if err := r.ops.VMM.DestroyInstance(ctx, id); err != nil {
		r.log.Error("runner: could not destroy the instance", "error", err)
	}
	for slot, tap := range r.st.Taps {
		if err := r.ops.Net.DestroyTap(ctx, id, tap); err != nil {
			r.log.Error("runner: could not destroy a tap", "tap", tap, "slot", slot, "error", err)
		}
	}
}
