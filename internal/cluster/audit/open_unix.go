//go:build unix

package audit

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// errNotRegular is a segment that is not a regular file (a symbolic link,
// a FIFO, a device, a directory): a break, never read (F8 of the 8b-2b
// review: audit verify runs as root over a directory the service account
// owns).
var errNotRegular = errors.New("not a regular file")

// openSegment opens a segment without following a symbolic link and
// without waiting on a FIFO or a device, and checks on the open file that
// it is a regular file.
func openSegment(path string, flag int) (*os.File, error) {
	f, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if info, lerr := os.Lstat(path); lerr == nil && !info.Mode().IsRegular() {
			return nil, errNotRegular
		}
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("audit: %w", err)
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, errNotRegular
	}
	return f, nil
}
