//go:build freebsd

package main

import (
	"log/slog"

	"github.com/greenbeanorg/skep/internal/hostops"
)

// newHostOps returns the real privileged operations for this FreeBSD
// host. A FreeBSD binary has no other kind: run as an ordinary user, the
// real operations fail on permissions, which is the correct outcome.
func newHostOps(conf hostops.Confinement, log *slog.Logger) (hostops.Ops, error) {
	return hostops.New(conf, hostops.Options{Logger: log})
}
