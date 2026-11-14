//go:build !freebsd

package main

import (
	"log/slog"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/hostops/hostfake"
)

// newHostOps returns the in-memory fake: this is a development machine,
// not a Skep host. Pools, bridges, and VMs are simulated and nothing on
// the machine is touched (ADR 0009 decision 1b).
func newHostOps(conf hostops.Confinement, log *slog.Logger) (hostops.Ops, error) {
	log.Warn("privileged host operations are simulated: this build is for development and runs no VM",
		"implementation", hostfake.Marker)
	return hostfake.New(conf, hostfake.Options{Logger: log}).Ops(), nil
}
