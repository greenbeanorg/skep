//go:build freebsd

package hostops

import (
	"bytes"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// The writes of decision 13. Every name is opened relative to a directory
// descriptor that descends from the jail root's, with O_RESOLVE_BENEATH, so
// a symbolic link anywhere in the path cannot lead outside the root (the
// kernel fails the lookup with ENOTCAPABLE), and with O_NOFOLLOW, so the
// last component is never a link. A file is replaced by writing a new one
// beside it and renaming it over the old name, which replaces a link
// rather than following it.

const beneath = unix.O_RESOLVE_BENEATH | unix.O_NOFOLLOW | unix.O_CLOEXEC

// The files Skep writes, relative to the jail root: etc/rc.conf,
// etc/resolv.conf, and root/.ssh/authorized_keys.
const (
	jailRCConfDir      = "etc"
	jailRCConfName     = "rc.conf"
	jailResolvConfName = "resolv.conf"
	jailRootHome       = "root"
	jailSSHDir         = ".ssh"
	jailAuthorizedKeys = "authorized_keys"
)

// writeJailFiles writes the rendered files under root, the jail root's
// path on the host (a mountpoint under the 0700 jail directory).
func writeJailFiles(root string, c jailContent) error {
	rfd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("hostops: open the jail root %s: %w", root, err)
	}
	defer unix.Close(rfd)

	etc, err := openDirBeneath(rfd, jailRCConfDir)
	if err != nil {
		return err
	}
	defer unix.Close(etc)
	if c.rcConf != nil {
		if err := replaceSkepFile(etc, jailRCConfName, c.rcConf, 0o644); err != nil {
			return err
		}
	} else if err := removeSkepFile(etc, jailRCConfName); err != nil {
		return err
	}
	// resolv.conf: Skep's own for static addressing. Without nameservers
	// only Skep's own is removed; any other (a template's) stays.
	if c.resolvConf != nil {
		if err := replaceSkepFile(etc, jailResolvConfName, c.resolvConf, 0o644); err != nil {
			return err
		}
	} else if head, missing, err := readSkepFile(etc, jailResolvConfName); err != nil {
		return err
	} else if !missing && bytes.Equal(head, []byte(RCConfHeader)) {
		if err := removeSkepFile(etc, jailResolvConfName); err != nil {
			return err
		}
	}

	home, err := openDirBeneath(rfd, jailRootHome)
	if err != nil {
		return err
	}
	defer unix.Close(home)
	if c.authorizedKeys != nil {
		ssh, err := ensureDirBeneath(home, jailSSHDir, 0o700)
		if err != nil {
			return err
		}
		defer unix.Close(ssh)
		return replaceFile(ssh, jailAuthorizedKeys, c.authorizedKeys, 0o600)
	}
	ssh, err := openDirBeneath(home, jailSSHDir)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(ssh)
	if err := unix.Unlinkat(ssh, jailAuthorizedKeys, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("hostops: remove root's authorized_keys: %w", err)
	}
	return unix.Fsync(ssh)
}

// openDirBeneath opens a directory below dirfd.
func openDirBeneath(dirfd int, name string) (int, error) {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|beneath, 0)
	if err != nil {
		return -1, fmt.Errorf("hostops: open %s in the jail: %w", name, err)
	}
	return fd, nil
}

// ensureDirBeneath creates a directory below dirfd if it is missing, and
// opens it, root:wheel with the given mode.
func ensureDirBeneath(dirfd int, name string, mode uint32) (int, error) {
	if err := unix.Mkdirat(dirfd, name, mode); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, fmt.Errorf("hostops: create %s in the jail: %w", name, err)
	}
	fd, err := openDirBeneath(dirfd, name)
	if err != nil {
		return -1, err
	}
	err = unix.Fchown(fd, 0, 0)
	if err == nil {
		err = unix.Fchmod(fd, mode)
	}
	if err == nil {
		err = unix.Fsync(dirfd)
	}
	if err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("hostops: set the owner and mode of %s in the jail: %w", name, err)
	}
	return fd, nil
}

// readSkepFile reads an existing file below dirfd. missing is true when
// there is none.
func readSkepFile(dirfd int, name string) (data []byte, missing bool, err error) {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|beneath, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("hostops: open %s in the jail: %w", name, err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, false, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, false, fmt.Errorf("hostops: %s in the jail is not a regular file", name)
	}
	buf := make([]byte, len(RCConfHeader))
	n, err := unix.Read(fd, buf)
	if err != nil {
		return nil, false, err
	}
	return buf[:n], false, nil
}

// replaceSkepFile replaces a file only if it is missing or Skep wrote it.
func replaceSkepFile(dirfd int, name string, data []byte, mode uint32) error {
	head, missing, err := readSkepFile(dirfd, name)
	if err != nil {
		return err
	}
	if !missing && !bytes.Equal(head, []byte(RCConfHeader)) {
		return fmt.Errorf("hostops: the jail's %s was not written by Skep; Skep does not replace it", name)
	}
	return replaceFile(dirfd, name, data, mode)
}

// removeSkepFile removes a file only if Skep wrote it.
func removeSkepFile(dirfd int, name string) error {
	head, missing, err := readSkepFile(dirfd, name)
	if err != nil || missing {
		return err
	}
	if !bytes.Equal(head, []byte(RCConfHeader)) {
		return fmt.Errorf("hostops: the jail's %s was not written by Skep; Skep does not remove it", name)
	}
	if err := unix.Unlinkat(dirfd, name, 0); err != nil {
		return err
	}
	return unix.Fsync(dirfd)
}

// replaceFile writes data to a new file beside name, root:wheel with the
// given mode, fsyncs it, and renames it over name.
func replaceFile(dirfd int, name string, data []byte, mode uint32) error {
	tmp := "." + name + ".skep-new"
	if err := unix.Unlinkat(dirfd, tmp, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("hostops: remove a stale %s in the jail: %w", tmp, err)
	}
	fd, err := unix.Openat(dirfd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|beneath, mode)
	if err != nil {
		return fmt.Errorf("hostops: create %s in the jail: %w", tmp, err)
	}
	err = unix.Fchown(fd, 0, 0)
	if err == nil {
		err = unix.Fchmod(fd, mode)
	}
	for rest := data; err == nil && len(rest) > 0; {
		var n int
		n, err = unix.Write(fd, rest)
		rest = rest[n:]
	}
	if err == nil {
		err = unix.Fsync(fd)
	}
	if cerr := unix.Close(fd); err == nil {
		err = cerr
	}
	if err == nil {
		err = unix.Renameat(dirfd, tmp, dirfd, name)
	}
	if err != nil {
		_ = unix.Unlinkat(dirfd, tmp, 0)
		return fmt.Errorf("hostops: write %s in the jail: %w", name, err)
	}
	return unix.Fsync(dirfd)
}
