// Package sysaccount resolves operating system user and group names to
// numeric IDs. Configuration names accounts ("skep"); the kernel reports
// and accepts numbers.
package sysaccount

import (
	"fmt"
	"os/user"
	"strconv"
)

// UID returns the numeric ID of a user given by name or as a number.
func UID(nameOrID string) (uint32, error) {
	if n, err := strconv.ParseUint(nameOrID, 10, 32); err == nil {
		return uint32(n), nil
	}
	u, err := user.Lookup(nameOrID)
	if err != nil {
		return 0, fmt.Errorf("user %q: %w", nameOrID, err)
	}
	n, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("user %q has a non-numeric UID %q", nameOrID, u.Uid)
	}
	return uint32(n), nil
}

// UIDs resolves a list of users, without duplicates.
func UIDs(namesOrIDs []string) ([]uint32, error) {
	var out []uint32
	seen := map[uint32]bool{}
	for _, v := range namesOrIDs {
		uid, err := UID(v)
		if err != nil {
			return nil, err
		}
		if !seen[uid] {
			seen[uid] = true
			out = append(out, uid)
		}
	}
	return out, nil
}

// GID returns the numeric ID of a group given by name or as a number.
func GID(nameOrID string) (uint32, error) {
	if n, err := strconv.ParseUint(nameOrID, 10, 32); err == nil {
		return uint32(n), nil
	}
	g, err := user.LookupGroup(nameOrID)
	if err != nil {
		return 0, fmt.Errorf("group %q: %w", nameOrID, err)
	}
	n, err := strconv.ParseUint(g.Gid, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("group %q has a non-numeric GID %q", nameOrID, g.Gid)
	}
	return uint32(n), nil
}

// Account is a user with its primary group.
type Account struct {
	UID uint32
	GID uint32
}

// Lookup returns the UID and primary GID of a user given by name.
func Lookup(name string) (Account, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return Account{}, fmt.Errorf("user %q: %w", name, err)
	}
	uid, err1 := strconv.ParseUint(u.Uid, 10, 32)
	gid, err2 := strconv.ParseUint(u.Gid, 10, 32)
	if err1 != nil || err2 != nil {
		return Account{}, fmt.Errorf("user %q has non-numeric IDs", name)
	}
	return Account{UID: uint32(uid), GID: uint32(gid)}, nil
}
