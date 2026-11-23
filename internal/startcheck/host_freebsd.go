//go:build freebsd

package startcheck

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const (
	getfaclBinary  = "/bin/getfacl"
	getfaclTimeout = 10 * time.Second
)

// Host is the running FreeBSD host.
func Host() FS { return hostFS{} }

type hostFS struct{}

func (hostFS) Lstat(path string) (Info, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return Info{}, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return Info{}, fmt.Errorf("no owner in the stat result of %s", path)
	}
	return Info{UID: sys.Uid, Perm: st.Mode().Perm(), Symlink: st.Mode()&fs.ModeSymlink != 0}, nil
}

func (hostFS) Readlink(path string) (string, error) { return os.Readlink(path) }

// ACL runs getfacl with fixed arguments and environment. path is absolute,
// so it cannot be read as an option.
func (hostFS) ACL(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("not an absolute path")
	}
	ctx, cancel := context.WithTimeout(context.Background(), getfaclTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, getfaclBinary, "-q", "-n", path)
	cmd.Env, cmd.Dir = []string{"PATH=/sbin:/bin:/usr/sbin:/usr/bin", "LC_ALL=C"}, "/"
	out, err := cmd.Output()
	return string(out), err
}
