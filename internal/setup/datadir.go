package setup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// dataDirChain is Skep's data directory and its parents, which setup
// checks before it reads or writes anything there (journals, database
// backups): a directory someone other than root can change lets them plant
// or swap what root later reads or writes.
var dataDirChain = []string{"/var/db/skep", "/var/db", "/var"}

// statUID returns the UID that owns a file, from stat(2).
func statUID(st fs.FileInfo) (uint32, bool) {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return sys.Uid, true
}

// checkDataDir refuses when /var/db/skep or a parent under root is not
// owned by root or is writable by its group or others. /var/db/skep must
// be a real directory (not a symbolic link); a missing /var/db/skep is
// fine (nothing is there yet), its parents must exist. It is applied to
// the real root only (CheckDataDir): with -root, which is for tests, the
// files belong to whoever runs them.
func checkDataDir(root string, uidOf func(fs.FileInfo) (uint32, bool)) error {
	for i, p := range dataDirChain {
		full := filepath.Join(root, p)
		var st fs.FileInfo
		var err error
		if i == 0 {
			st, err = os.Lstat(full)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
		} else {
			st, err = os.Stat(full)
		}
		switch {
		case err != nil:
			return fmt.Errorf("%s: %w; setup will not use Skep's data directory until this is resolved", p, err)
		case st.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s is a symbolic link; setup will not use it (make it a directory owned by root)", p)
		case !st.IsDir():
			return fmt.Errorf("%s is not a directory; setup will not use it", p)
		}
		uid, ok := uidOf(st)
		if !ok || uid != 0 {
			return fmt.Errorf("%s is owned by UID %d, not root; setup will not use Skep's data directory (chown root:wheel %s)", p, uid, p)
		}
		if perm := st.Mode().Perm(); perm&0o022 != 0 {
			return fmt.Errorf("%s is writable by its group or others (%04o); setup will not use Skep's data directory (chmod go-w %s)", p, perm, p)
		}
	}
	return nil
}

// CheckDataDir is checkDataDir on the real root; for any other root it
// does nothing.
func CheckDataDir(root string) error {
	if filepath.Clean(root) != "/" {
		return nil
	}
	return checkDataDir("/", statUID)
}
