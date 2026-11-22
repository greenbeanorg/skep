//go:build !unix

package setup

import (
	"errors"
	"syscall"
)

// sysProcAttr: setup runs commands on FreeBSD only.
func sysProcAttr(Cmd) (*syscall.SysProcAttr, error) {
	return nil, errors.New("skep-setup runs commands on FreeBSD only")
}
