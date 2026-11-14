package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/hypervisor/bhyve"
)

// runRunner is "skep-agent vm-runner": the per-VM process that owns a
// running bhyve guest (ADR 0009 decision 7a). The agent starts it in a
// session of its own; it is not meant to be run by hand. Everything it
// needs is in the spec the agent wrote in the VM's root-only runtime
// directory.
func runRunner(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("skep-agent "+hostops.RunnerCommand, flag.ContinueOnError)
	flags.SetOutput(stderr)
	vmID := flags.String("vm", "", "ID of the VM to run")
	runDir := flags.String("run-dir", "", "the agent's VM runtime directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if !hostops.ValidID(*vmID) || !filepath.IsAbs(*runDir) || filepath.Clean(*runDir) != *runDir {
		return errors.New("vm-runner: -vm must be a VM ID and -run-dir an absolute, clean path")
	}
	if os.Geteuid() != 0 {
		return errors.New("vm-runner: runs as root only, started by the agent")
	}
	raw, err := os.ReadFile(filepath.Join(*runDir, *vmID, hostops.RunSpecFile))
	if err != nil {
		return fmt.Errorf("vm-runner: %w", err)
	}
	var spec bhyve.Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return fmt.Errorf("vm-runner: the spec is not valid: %w", err)
	}
	if spec.Conf.RunDir != *runDir || spec.VMID != *vmID {
		return errors.New("vm-runner: the spec does not match the arguments")
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil)).With("component", "vm-runner")
	ops, err := runnerOps(spec.Conf, log)
	if err != nil {
		return err
	}

	// The agent's requests arrive as signals. Terminal signals do nothing:
	// the runner has no terminal, and an accidental one must not stop a VM.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGUSR1)
	signal.Ignore(syscall.SIGINT, syscall.SIGHUP, syscall.SIGPIPE)
	requests := make(chan hostops.RunnerSignal, 4)
	go func() {
		for s := range sigs {
			switch s {
			case syscall.SIGTERM:
				requests <- hostops.StopGraceful
			case syscall.SIGUSR1:
				requests <- hostops.StopForce
			}
		}
	}()
	log.Info("runner starting", "vm_id", *vmID, "pid", os.Getpid())
	// Not the agent's signal context: only the requests above end a run.
	return bhyve.RunVM(context.Background(), ops, *vmID, os.Getpid(), requests, log)
}
