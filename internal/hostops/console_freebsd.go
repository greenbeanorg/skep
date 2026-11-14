//go:build freebsd

package hostops

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// OpenConsole implements VMM. The B side is put in raw mode, with CLOCAL
// so that neither open nor read waits for a carrier: bytes pass through
// unchanged in both directions, and the terminal emulation is the
// browser's.
func (h *host) OpenConsole(vmID string) (io.ReadWriteCloser, error) {
	path, err := ConsoleDevice(vmID, 'B')
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("hostops: open the console of VM %s: %w", vmID, err)
	}
	t, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err == nil {
		t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
		t.Oflag &^= unix.OPOST
		t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
		t.Cflag &^= unix.CSIZE | unix.PARENB
		t.Cflag |= unix.CS8 | unix.CLOCAL | unix.CREAD
		t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 1, 0
		err = unix.IoctlSetTermios(fd, unix.TIOCSETA, t)
	}
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("hostops: set the console of VM %s to raw mode: %w", vmID, err)
	}
	// A non-blocking descriptor goes to the runtime's poller, so Close
	// interrupts a pending Read.
	return os.NewFile(uintptr(fd), path), nil
}
