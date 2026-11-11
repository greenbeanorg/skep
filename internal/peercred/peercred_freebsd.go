//go:build freebsd

package peercred

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// xucredVersion is XUCRED_VERSION from <sys/ucred.h>.
const xucredVersion = 0

// fromFD reads the peer's struct xucred with getsockopt(LOCAL_PEERCRED).
func fromFD(fd int) (Cred, error) {
	xu, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return Cred{}, fmt.Errorf("getsockopt LOCAL_PEERCRED: %w", err)
	}
	if xu.Version != xucredVersion {
		return Cred{}, fmt.Errorf("unexpected xucred version %d", xu.Version)
	}
	if xu.Ngroups < 1 {
		return Cred{}, fmt.Errorf("xucred carries no groups")
	}
	// cr_groups[0] is the effective group ID.
	return Cred{UID: xu.Uid, GID: xu.Groups[0]}, nil
}
