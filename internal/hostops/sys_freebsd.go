//go:build freebsd

package hostops

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// New returns the real implementation for this FreeBSD host.
func New(conf Confinement, opts Options) (Ops, error) {
	timeout := opts.CommandTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	h := newHost(conf, execRunner{timeout: timeout}, kernel{}, opts.Logger)
	return Ops{Conf: conf, ZFS: h, Net: h, VMM: h, Files: h}, nil
}

// kernel is the real system binding.
type kernel struct{}

func (kernel) SysctlUint64(name string) (uint64, error) {
	raw, err := unix.SysctlRaw(name)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return 0, errNoSuchOID
		}
		return 0, err
	}
	switch len(raw) {
	case 4:
		return uint64(binary.NativeEndian.Uint32(raw)), nil
	case 8:
		return binary.NativeEndian.Uint64(raw), nil
	default:
		return 0, fmt.Errorf("unexpected integer size of %d bytes", len(raw))
	}
}

func (kernel) SysctlExists(name string) (bool, error) {
	_, err := unix.SysctlRaw(name)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.ENOENT):
		return false, nil
	}
	return false, err
}

func (kernel) Exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
