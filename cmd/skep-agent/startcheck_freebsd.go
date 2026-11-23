//go:build freebsd

package main

import (
	"os"

	"github.com/greenbeanorg/skep/internal/startcheck"
)

// startFS is the host to check: only an agent running as root checks.
func startFS() (startcheck.FS, bool) {
	if os.Geteuid() != 0 {
		return nil, false
	}
	return startcheck.Host(), true
}
