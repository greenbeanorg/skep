//go:build unix

package setup

import "syscall"

// sysProcAttr puts a non-interactive command in its own process group and
// sets the user a command runs as.
func sysProcAttr(c Cmd) (*syscall.SysProcAttr, error) {
	attr := &syscall.SysProcAttr{Setpgid: !c.Interactive}
	if c.As != nil {
		attr.Credential = &syscall.Credential{Uid: c.As.UID, Gid: c.As.GID, Groups: []uint32{}}
	}
	return attr, nil
}
