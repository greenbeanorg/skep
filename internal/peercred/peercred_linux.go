//go:build linux

package peercred

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// fromFD reads the peer's struct ucred with getsockopt(SO_PEERCRED). Linux
// is a development platform only.
func fromFD(fd int) (Cred, error) {
	uc, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return Cred{}, fmt.Errorf("getsockopt SO_PEERCRED: %w", err)
	}
	return Cred{UID: uc.Uid, GID: uc.Gid}, nil
}
