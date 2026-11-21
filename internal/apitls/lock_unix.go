//go:build linux || freebsd || darwin

package apitls

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockDir takes an flock(2) advisory lock on the TLS directory itself:
// shared for reading the pair, exclusive for changing it. No lock file is
// created. The lock belongs to the open directory, so it serializes
// goroutines of one process as well as separate processes (skep-api
// reading, 'skep-api tls regenerate' writing), and the kernel releases it
// when a holder exits or crashes. unlock releases it.
func lockDir(dir string, exclusive bool) (unlock func(), err error) {
	d, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	for {
		err = syscall.Flock(int(d.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("apitls: lock %s: %w", dir, err)
	}
	// Closing the directory releases the lock.
	return func() { d.Close() }, nil
}
