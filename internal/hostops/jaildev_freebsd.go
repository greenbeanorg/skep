//go:build freebsd

package hostops

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// listJailDev lists the entries of a jail's /dev from the host (amendment
// i). The jail root is opened without following a link, dev is opened
// beneath it without following one, and the directory must be a devfs
// mounted exactly at <root>/dev: a jail whose dev is a link or not devfs
// is refused. Only the top level is listed; pts and fd are directories of
// the allow-list.
func listJailDev(root string) ([]string, error) {
	rfd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("hostops: open the jail root %s: %w", root, err)
	}
	defer unix.Close(rfd)
	dfd, err := openDirBeneath(rfd, "dev")
	if err != nil {
		return nil, err
	}
	d := os.NewFile(uintptr(dfd), filepath.Join(root, "dev"))
	defer d.Close()
	var st unix.Statfs_t
	if err := unix.Fstatfs(dfd, &st); err != nil {
		return nil, fmt.Errorf("hostops: statfs the jail's /dev: %w", err)
	}
	fstype, on := unix.ByteSliceToString(st.Fstypename[:]), unix.ByteSliceToString(st.Mntonname[:])
	if want := filepath.Join(root, "dev"); fstype != "devfs" || on != want {
		return nil, fmt.Errorf("hostops: the jail's /dev is %s mounted on %s, not devfs on %s", printable(fstype), printable(on), want)
	}
	return d.Readdirnames(-1)
}
