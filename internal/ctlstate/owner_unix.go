//go:build unix

package ctlstate

import (
	"fmt"
	"io/fs"
	"syscall"
)

// checkOwner refuses a file or directory that belongs to another user.
func (s *Store) checkOwner(path string, info fs.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot read the owner of %s", path)
	}
	if int(st.Uid) != s.uid {
		return fmt.Errorf("%s belongs to uid %d, not to you (uid %d), and is not used", path, st.Uid, s.uid)
	}
	return nil
}
