//go:build unix

package audit

import (
	"os"
	"syscall"
)

// flockExclusive takes an exclusive flock on f without waiting.
func flockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
