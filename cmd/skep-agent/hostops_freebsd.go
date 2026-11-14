//go:build freebsd

package main

import (
	"log/slog"
	"os"

	"github.com/greenbeanorg/skep/internal/hostops"
)

// newHostOps returns the real privileged operations for this FreeBSD
// host. A FreeBSD binary has no other kind: run as an ordinary user, the
// real operations fail on permissions, which is the correct outcome.
func newHostOps(conf hostops.Confinement, log *slog.Logger) (hostops.Ops, error) {
	self, err := os.Executable()
	if err != nil {
		log.Warn("the agent's own executable is not known: no VM can be started", "error", err)
		self = ""
	}
	return hostops.New(conf, hostops.Options{Logger: log, Self: self})
}

// runnerOps is what a runner uses: the same real operations.
func runnerOps(conf hostops.Confinement, log *slog.Logger) (hostops.Ops, error) {
	return hostops.New(conf, hostops.Options{Logger: log})
}
