//go:build !freebsd

package setup

import "github.com/greenbeanorg/skep/internal/startcheck"

// dataDirFS: the ACL rules read FreeBSD ACLs and apply on FreeBSD only.
func dataDirFS() (startcheck.FS, bool) { return nil, false }
