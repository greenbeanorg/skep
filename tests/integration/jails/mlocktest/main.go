//go:build freebsd

// Command mlocktest locks N MiB with mlock(2), optionally holds the lock
// for some seconds, and reports the result. It is the live check that a
// jail's limits.locked_memory (RCTL memorylocked) refuses an mlock past
// it (ADR 0010 amendment j). It is copied into a test jail by hand for
// that check and is not part of Skep.
//
// Usage: mlocktest MiB [hold-seconds]
package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

// mlock and munlock call the system calls directly: the syscall package
// has no wrappers for them on FreeBSD.
func mlock(b []byte) error   { return call(syscall.SYS_MLOCK, b) }
func munlock(b []byte) error { return call(syscall.SYS_MUNLOCK, b) }

func call(trap uintptr, b []byte) error {
	_, _, errno := syscall.Syscall(trap, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mlocktest MiB [hold-seconds]")
		os.Exit(2)
	}
	mib, err := strconv.Atoi(os.Args[1])
	if err != nil || mib < 1 || mib > 4096 {
		fmt.Fprintln(os.Stderr, "mlocktest: MiB must be 1 to 4096")
		os.Exit(2)
	}
	hold := 0
	if len(os.Args) > 2 {
		if hold, err = strconv.Atoi(os.Args[2]); err != nil || hold < 0 || hold > 600 {
			fmt.Fprintln(os.Stderr, "mlocktest: hold-seconds must be 0 to 600")
			os.Exit(2)
		}
	}
	b := make([]byte, mib<<20)
	for i := 0; i < len(b); i += os.Getpagesize() {
		b[i] = 1
	}
	if err := mlock(b); err != nil {
		errno, _ := err.(syscall.Errno)
		fmt.Printf("mlock %d MiB: refused: %v (errno %d)\n", mib, err, int(errno))
		os.Exit(1)
	}
	fmt.Printf("mlock %d MiB: locked\n", mib)
	time.Sleep(time.Duration(hold) * time.Second)
	if err := munlock(b); err != nil {
		fmt.Printf("munlock: %v\n", err)
		os.Exit(1)
	}
}
