//go:build !freebsd

package hostops

import (
	"errors"
	"runtime"
)

// kernelMounts exists only on FreeBSD (getfsstat, MNT_BYFSID).
// Development machines use the hostfake package.
type kernelMounts struct{}

// Mounts implements mountTable.
func (kernelMounts) Mounts() ([]fsMount, error) {
	return nil, errors.New("hostops: reading the mount table exists only on FreeBSD, not on " + runtime.GOOS)
}

// UnmountFSID implements mountTable.
func (kernelMounts) UnmountFSID([2]int32) error {
	return errors.New("hostops: unmounting by fsid exists only on FreeBSD, not on " + runtime.GOOS)
}
