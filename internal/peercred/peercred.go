// Package peercred reads the credentials of the process on the other end of
// a Unix domain socket, as reported by the kernel.
//
// The agent uses this to decide who may talk to it over its local socket
// (see docs/decisions/0004-privilege-boundary.md). The credentials come
// from the kernel, not from anything the peer sends, so the peer cannot
// forge them. They describe the process that called connect(2).
package peercred

import (
	"errors"
	"fmt"
	"net"
	"slices"
)

// ErrUnsupported is returned on operating systems where peer credentials
// are not implemented. Callers must treat it as "deny".
var ErrUnsupported = errors.New("peercred: not supported on this operating system")

// Cred is the kernel-reported identity of a socket peer.
type Cred struct {
	// UID is the effective user ID of the peer.
	UID uint32
	// GID is the effective (primary) group ID of the peer.
	GID uint32
}

// FromConn returns the credentials of the peer of a Unix domain socket.
func FromConn(conn *net.UnixConn) (Cred, error) {
	if conn == nil {
		return Cred{}, errors.New("peercred: nil connection")
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return Cred{}, fmt.Errorf("peercred: %w", err)
	}
	var (
		cred    Cred
		credErr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = fromFD(int(fd))
	}); err != nil {
		return Cred{}, fmt.Errorf("peercred: %w", err)
	}
	if credErr != nil {
		return Cred{}, fmt.Errorf("peercred: %w", credErr)
	}
	return cred, nil
}

// Allowed reports whether cred's UID is in the allow-list. An empty list
// allows nobody.
func Allowed(cred Cred, uids []uint32) bool {
	return slices.Contains(uids, cred.UID)
}
