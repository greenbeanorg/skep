//go:build freebsd

package setup

import (
	"os"

	"github.com/greenbeanorg/skep/internal/startcheck"
)

// dataDirFS is the host for the ACL rules. Only root applies them: without
// root an ACL may be unreadable, which would refuse a healthy directory.
func dataDirFS() (startcheck.FS, bool) {
	if os.Geteuid() != 0 {
		return nil, false
	}
	return startcheck.Host(), true
}
