//go:build !freebsd

package hostops

import (
	"errors"
	"runtime"
)

// New fails everywhere but on FreeBSD: there is no real implementation of
// privileged host operations for any other system. Development machines
// use the hostfake package instead.
func New(Confinement, Options) (Ops, error) {
	return Ops{}, errors.New("hostops: privileged host operations exist only on FreeBSD, not on " + runtime.GOOS)
}
