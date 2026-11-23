//go:build !freebsd

package main

import "github.com/greenbeanorg/skep/internal/startcheck"

// startFS: a Linux development agent runs as the developer from the source
// tree and does not check.
func startFS() (startcheck.FS, bool) { return nil, false }
