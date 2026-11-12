package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

const ioctlGetTermios = unix.TIOCGETA

// openPTY opens a pseudo-terminal pair through pts(4).
func openPTY() (master, slave *os.File, err error) {
	// x/sys/unix has no wrapper for posix_openpt(2).
	r, _, errno := unix.Syscall(unix.SYS_POSIX_OPENPT, uintptr(unix.O_RDWR|unix.O_NOCTTY), 0, 0)
	if errno != 0 {
		return nil, nil, errno
	}
	fd := int(r)
	master = os.NewFile(uintptr(fd), "pty-master")
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}
