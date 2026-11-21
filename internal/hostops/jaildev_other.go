//go:build !freebsd

package hostops

import (
	"errors"
	"runtime"
)

// listJailDev exists only on FreeBSD (devfs). Development machines use
// the hostfake package.
func listJailDev(string) ([]string, error) {
	return nil, errors.New("hostops: listing a jail's devfs exists only on FreeBSD, not on " + runtime.GOOS)
}
