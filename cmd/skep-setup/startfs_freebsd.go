//go:build freebsd

package main

import "github.com/greenbeanorg/skep/internal/startcheck"

// hostStartFS is the host the agent's start check reads.
func hostStartFS() (startcheck.FS, bool) { return startcheck.Host(), true }
