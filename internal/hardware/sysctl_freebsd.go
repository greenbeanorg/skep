//go:build freebsd

package hardware

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// New returns the native FreeBSD Discoverer. It reads sysctl through
// sysctl(3), not by running sysctl(8).
func New(opts Options) Discoverer {
	return &freebsdDiscoverer{
		sysctl: unixSysctl{},
		exists: pathExists,
		find:   findInDirs(searchDirs),
		run:    execRunner{timeout: opts.commandTimeout()},
		now:    time.Now,
	}
}

// pathExists reports whether path exists on the real filesystem.
func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// unixSysctl is the real SysctlReader.
type unixSysctl struct{}

func (unixSysctl) String(name string) (string, error) {
	v, err := unix.Sysctl(name)
	if err != nil {
		return "", mapSysctlErr(err)
	}
	return v, nil
}

// Uint64 reads the raw value and decodes it by size, because integer
// sysctls are 32 or 64 bits wide depending on the OID.
func (unixSysctl) Uint64(name string) (uint64, error) {
	raw, err := unix.SysctlRaw(name)
	if err != nil {
		return 0, mapSysctlErr(err)
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

func mapSysctlErr(err error) error {
	if errors.Is(err, unix.ENOENT) {
		return ErrSysctlNotFound
	}
	return err
}
