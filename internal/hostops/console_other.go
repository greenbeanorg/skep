//go:build !freebsd

package hostops

import (
	"errors"
	"io"
)

// OpenConsole implements VMM: nmdm(4) exists on FreeBSD only.
func (h *host) OpenConsole(string) (io.ReadWriteCloser, error) {
	return nil, errors.New("hostops: the serial console exists on FreeBSD only")
}
