//go:build !freebsd

package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/hostops/hostfake"
	"github.com/greenbeanorg/skep/internal/hypervisor/bhyve"
)

// newHostOps returns the in-memory fake: this is a development machine,
// not a Skep host. Pools, bridges, and VMs are simulated, the runner loop
// runs in a goroutine against the fake, and nothing on the machine is
// touched (ADR 0009 decision 1b).
func newHostOps(conf hostops.Confinement, log *slog.Logger) (hostops.Ops, error) {
	log.Warn("privileged host operations are simulated: this build is for development and runs no VM",
		"implementation", hostfake.Marker)
	return hostfake.New(conf, hostfake.Options{Logger: log, Runner: fakeRunner(log)}).Ops(), nil
}

// fakeRunner runs the real runner loop against the fake.
func fakeRunner(log *slog.Logger) hostfake.RunnerFunc {
	return func(ctx context.Context, ops hostops.Ops, vmID string, pid int, signals <-chan hostops.RunnerSignal) error {
		return bhyve.RunVM(ctx, ops, vmID, pid, signals, log)
	}
}

// runnerOps: a runner process exists only on FreeBSD.
func runnerOps(hostops.Confinement, *slog.Logger) (hostops.Ops, error) {
	return hostops.Ops{}, errors.New("vm-runner: runs on FreeBSD only; development builds run the runner loop inside the agent")
}
