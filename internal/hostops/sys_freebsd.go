//go:build freebsd

package hostops

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// New returns the real implementation for this FreeBSD host.
func New(conf Confinement, opts Options) (Ops, error) {
	timeout := opts.CommandTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	h := newHost(conf, execRunner{timeout: timeout}, kernel{}, opts.Logger)
	h.spawn, h.self = execSpawner{}, opts.Self
	return Ops{Conf: conf, ZFS: h, Dirs: newDirs(conf, kernel{}, opts.Logger), Net: h, VMM: h, Files: h, Procs: h, Templates: h, Jails: h}, nil
}

// kernel is the real system binding.
type kernel struct{}

func (kernel) SysctlUint64(name string) (uint64, error) {
	raw, err := unix.SysctlRaw(name)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return 0, errNoSuchOID
		}
		return 0, err
	}
	switch len(raw) {
	case 4:
		return uint64(binary.NativeEndian.Uint32(raw)), nil
	case 8:
		return binary.NativeEndian.Uint64(raw), nil
	default:
		return 0, fmt.Errorf("unexpected integer size of %d bytes", len(raw))
	}
}

func (kernel) SysctlExists(name string) (bool, error) {
	_, err := unix.SysctlRaw(name)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.ENOENT):
		return false, nil
	}
	return false, err
}

func (kernel) Exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func (kernel) Alive(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

// ProcessArgs reads kern.proc.args.<pid>: the arguments, each followed by
// a NUL byte.
func (kernel) ProcessArgs(pid int) ([]string, error) {
	raw, err := unix.SysctlRaw("kern.proc.args", pid)
	if err != nil {
		return nil, err
	}
	raw = bytes.TrimRight(raw, "\x00")
	if len(raw) == 0 {
		return nil, nil
	}
	return strings.Split(string(raw), "\x00"), nil
}

func (kernel) Kill(pid int, sig syscall.Signal) error { return unix.Kill(pid, sig) }

// StatDir reads a directory pool's directory with lstat(2), never
// following a final symbolic link, and its file system with statfs(2).
func (kernel) StatDir(path string) (DirStat, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return DirStat{}, err
	}
	st := DirStat{Mode: fi.Mode()}
	id, ok := fileIdentity(fi)
	if !ok {
		return DirStat{}, fmt.Errorf("lstat %s: no owner", path)
	}
	st.UID, st.Dev, st.Ino = id.UID, id.Dev, id.Ino
	if !fi.IsDir() {
		return st, nil
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return DirStat{}, err
	}
	st.FSType = unix.ByteSliceToString(fs.Fstypename[:])
	bs := int64(fs.Bsize)
	st.SizeBytes, st.FreeBytes = int64(fs.Blocks)*bs, int64(fs.Bfree)*bs
	st.AvailableBytes = max(fs.Bavail*bs, 0)
	return st, nil
}
