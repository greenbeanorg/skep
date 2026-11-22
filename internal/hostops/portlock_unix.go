//go:build unix

package hostops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// PortLockPath is the lock file of an owner's NIC slot: port<slot>.lock in
// the owner's runtime directory (ADR 0011 addition a). Both runtime
// directories are root-only and gone after a reboot.
func (c Confinement) PortLockPath(kind PortOwner, ownerID string, slot int) (string, error) {
	if !ValidID(ownerID) {
		return "", invalid("invalid owner ID")
	}
	if slot < 0 || slot > MaxSlot {
		return "", invalid("invalid NIC slot")
	}
	var dir string
	switch kind {
	case PortJail:
		dir = c.JailRunDir
	case PortVM:
		dir = c.RunDir
	default:
		return "", invalid("invalid owner kind")
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("hostops: the %s runtime directory is not configured", kind)
	}
	return filepath.Join(dir, ownerID, "port"+strconv.Itoa(slot)+".lock"), nil
}

// LockPort takes an flock(2) on the port's lock file, which serialises jail
// start, the VM runner (another process), and the health check. The kernel
// releases it when the holder exits.
func (c Confinement) LockPort(kind PortOwner, ownerID string, slot int, wait bool) (func(), error) {
	path, err := c.PortLockPath(kind, ownerID, slot)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("hostops: create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("hostops: open the port lock: %w", err)
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	for {
		err = syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrPortBusy
		}
		return nil, fmt.Errorf("hostops: lock %s: %w", path, err)
	}
	return func() { f.Close() }, nil
}

// LockPort implements PortFilters.
func (h *host) LockPort(kind PortOwner, ownerID string, slot int, wait bool) (func(), error) {
	return h.conf.LockPort(kind, ownerID, slot, wait)
}
