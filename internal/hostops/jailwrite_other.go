//go:build !freebsd

package hostops

import (
	"errors"
	"runtime"
)

// writeJailFiles exists only on FreeBSD: it relies on O_RESOLVE_BENEATH
// (decision 13). Development machines use the hostfake package.
func writeJailFiles(string, jailContent) error {
	return errors.New("hostops: writing into a jail exists only on FreeBSD, not on " + runtime.GOOS)
}
