//go:build freebsd

package hostops

import (
	"golang.org/x/sys/unix"
)

// kernelMounts is the host's mount table through getfsstat(2) and
// unmount(2) with MNT_BYFSID (amendment l).
type kernelMounts struct{}

// Mounts implements mountTable. MNT_NOWAIT returns the kernel's cached
// entries without asking each file system, which a hung one could stall;
// the fields read here do not change after the mount.
func (kernelMounts) Mounts() ([]fsMount, error) {
	for {
		n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
		if err != nil {
			return nil, err
		}
		// Room for mounts made between the two calls; a full buffer
		// means there may be more, so it is read again.
		buf := make([]unix.Statfs_t, n+8)
		got, err := unix.Getfsstat(buf, unix.MNT_NOWAIT)
		if err != nil {
			return nil, err
		}
		if got == len(buf) {
			continue
		}
		out := make([]fsMount, 0, got)
		for _, s := range buf[:got] {
			out = append(out, fsMount{
				On:     unix.ByteSliceToString(s.Mntonname[:]),
				FSType: unix.ByteSliceToString(s.Fstypename[:]),
				FSID:   s.Fsid.Val,
			})
		}
		return out, nil
	}
}

// UnmountFSID implements mountTable: unmount(2) of "FSID:<a>:<b>" with
// MNT_BYFSID, so no path is looked up.
func (kernelMounts) UnmountFSID(fsid [2]int32) error {
	return unix.Unmount(fsidString(fsid), unix.MNT_BYFSID)
}
