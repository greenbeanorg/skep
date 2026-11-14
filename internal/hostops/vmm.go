package hostops

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"syscall"
)

// DestroyInstance implements VMM.
func (h *host) DestroyInstance(ctx context.Context, vmID string) error {
	name, err := Instance(vmID)
	if err != nil {
		return err
	}
	if !h.InstanceExists(vmID) {
		return nil
	}
	if _, err := h.run.Run(ctx, bhyvectlBinary, "--vm="+name, "--destroy"); err != nil {
		if !h.InstanceExists(vmID) {
			return nil
		}
		return fmt.Errorf("hostops: destroy the instance of VM %s: %w", vmID, err)
	}
	return nil
}

// ForcePoweroff implements VMM.
func (h *host) ForcePoweroff(ctx context.Context, vmID string) error {
	name, err := Instance(vmID)
	if err != nil {
		return err
	}
	if !h.InstanceExists(vmID) {
		return nil
	}
	if _, err := h.run.Run(ctx, bhyvectlBinary, "--vm="+name, "--force-poweroff"); err != nil {
		return fmt.Errorf("hostops: power off the instance of VM %s: %w", vmID, err)
	}
	return nil
}

// spawner starts long-lived child processes: the runner, from the agent,
// and bhyve, from the runner. The FreeBSD binding is in spawn_freebsd.go.
type spawner interface {
	Spawn(path string, args []string, opts spawnOptions) (Process, error)
}

type spawnOptions struct {
	// NewSession puts the child in a session of its own, so it outlives
	// its parent and receives none of its parent's terminal signals.
	NewSession bool
	// Output receives the child's stdout and stderr; empty is /dev/null.
	Output string
	// Truncate empties Output first; otherwise it is appended to.
	Truncate bool
}

// StartBhyve implements VMM.
func (h *host) StartBhyve(_ context.Context, vmID string) (Process, error) {
	name, err := Instance(vmID)
	if err != nil {
		return nil, err
	}
	conf, err := h.StateFilePath(vmID, BhyveConfigFile)
	if err != nil {
		return nil, err
	}
	logPath, err := h.StateFilePath(vmID, BhyveLogFile)
	if err != nil {
		return nil, err
	}
	if h.spawn == nil {
		return nil, fmt.Errorf("hostops: starting processes is not available here")
	}
	// The configuration is a root-only file: nothing about the VM shows
	// in ps beyond the instance name (ADR 0009 decision 7a).
	return h.spawn.Spawn(bhyveBinary, []string{"-k", conf, name}, spawnOptions{Output: logPath, Truncate: true})
}

// RunnerCommand is the subcommand of skep-agent that runs a VM.
const RunnerCommand = "vm-runner"

// runnerArgs is the argument list a VM's runner is started with. It is
// also how a live runner is recognised.
func (h *host) runnerArgs(vmID string) []string {
	return []string{RunnerCommand, "-vm", vmID, "-run-dir", h.conf.RunDir}
}

// StartRunner implements Procs.
func (h *host) StartRunner(ctx context.Context, vmID string) (int, error) {
	if err := checkVMID(vmID); err != nil {
		return 0, err
	}
	if h.self == "" || h.spawn == nil {
		return 0, fmt.Errorf("hostops: the agent's own executable is not known; it cannot start a runner")
	}
	if pid, ok := h.Runner(vmID); ok {
		return 0, fmt.Errorf("hostops: VM %s already has a runner (pid %d)", vmID, pid)
	}
	if err := h.PrepareRun(ctx, vmID); err != nil {
		return 0, err
	}
	logPath, err := h.runFile(vmID, RunnerLogFile)
	if err != nil {
		return 0, err
	}
	p, err := h.spawn.Spawn(h.self, h.runnerArgs(vmID), spawnOptions{NewSession: true, Output: logPath})
	if err != nil {
		return 0, fmt.Errorf("hostops: start the runner of VM %s: %w", vmID, err)
	}
	pid := p.PID()
	// The agent reaps its own child while it lives; after an agent
	// restart, init does.
	go p.Wait()
	if err := h.WriteRunFile(vmID, RunnerPIDFile, []byte(strconv.Itoa(pid)+"\n")); err != nil {
		_ = p.Signal(syscall.SIGKILL)
		return 0, err
	}
	return pid, nil
}

// Runner implements Procs.
func (h *host) Runner(vmID string) (int, bool) {
	raw, err := h.ReadRunFile(vmID, RunnerPIDFile)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		return 0, false
	}
	if !h.sys.Alive(pid) {
		return 0, false
	}
	args, err := h.sys.ProcessArgs(pid)
	if err != nil || !isRunner(args, h.runnerArgs(vmID)) {
		// A recycled PID, or a process that is not this VM's runner.
		return 0, false
	}
	return pid, true
}

// isRunner compares a process's arguments, without argv[0], with what a
// runner is started with.
func isRunner(args, want []string) bool {
	if len(args) != len(want)+1 {
		return false
	}
	for i, w := range want {
		if args[i+1] != w {
			return false
		}
	}
	return true
}

// SignalRunner implements Procs.
func (h *host) SignalRunner(vmID string, sig RunnerSignal) error {
	var s syscall.Signal
	switch sig {
	case StopGraceful:
		s = syscall.SIGTERM
	case StopForce:
		s = syscall.SIGUSR1
	default:
		return fmt.Errorf("hostops: unknown runner signal %d", sig)
	}
	pid, ok := h.Runner(vmID)
	if !ok {
		return fmt.Errorf("hostops: VM %s has no runner", vmID)
	}
	if err := h.sys.Kill(pid, s); err != nil {
		return fmt.Errorf("hostops: signal the runner of VM %s: %w", vmID, err)
	}
	return nil
}
