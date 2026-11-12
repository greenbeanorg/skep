//go:build unix

package privdrop

import (
	"os"
	"testing"
)

func TestNeverDropsToRoot(t *testing.T) {
	if err := To(0, 0); err == nil {
		t.Fatal("To(0, 0) must be refused")
	}
}

// TestUnprivilegedCannotChangeIdentity runs as an ordinary user, where
// every step of the drop must fail and leave the process as it was. The
// real drop, from root, is checked on FreeBSD by the native tests.
func TestUnprivilegedCannotChangeIdentity(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	uid := os.Getuid()
	if err := To(uint32(uid)+1, uint32(os.Getgid())+1); err == nil {
		t.Fatal("an unprivileged process changed its identity")
	}
	if os.Getuid() != uid || os.Geteuid() != uid {
		t.Fatal("the process identity changed")
	}
}
