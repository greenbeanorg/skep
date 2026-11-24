//go:build !freebsd

package main

import "github.com/greenbeanorg/skep/internal/startcheck"

// hostStartFS: the start check reads FreeBSD ACLs and runs on FreeBSD only.
func hostStartFS() (startcheck.FS, bool) { return nil, false }
