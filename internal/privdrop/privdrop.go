//go:build unix

// Package privdrop gives up root for good. skep-api's local commands use it
// so that a command started with sudo opens the API's state as the service
// account, never as root (ADR 0006 decision 8).
package privdrop

import (
	"errors"
	"fmt"
	"syscall"
)

// To changes the process to the given user and group, with no
// supplementary groups, and checks that root cannot be regained. It must
// be called before the process opens anything or starts other goroutines
// that matter.
func To(uid, gid uint32) error {
	if uid == 0 {
		return errors.New("privdrop: refusing to drop to root")
	}
	if err := syscall.Setgroups([]int{int(gid)}); err != nil {
		return fmt.Errorf("privdrop: setgroups: %w", err)
	}
	if err := syscall.Setgid(int(gid)); err != nil {
		return fmt.Errorf("privdrop: setgid: %w", err)
	}
	if err := syscall.Setuid(int(uid)); err != nil {
		return fmt.Errorf("privdrop: setuid: %w", err)
	}
	if syscall.Getuid() != int(uid) || syscall.Geteuid() != int(uid) ||
		syscall.Getgid() != int(gid) || syscall.Getegid() != int(gid) {
		return errors.New("privdrop: the process did not take the new identity")
	}
	if err := syscall.Setuid(0); err == nil {
		return errors.New("privdrop: root could be regained after the drop")
	}
	return nil
}
