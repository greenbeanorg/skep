package clusterstate

import (
	"fmt"
	"net/netip"
	"regexp"
)

var (
	nodeIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	nameRE   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	pinRE    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ValidNodeID reports whether id has the form of a node ID: a lower-case
// UUID, as skep-agent creates it.
func ValidNodeID(id string) bool { return nodeIDRE.MatchString(id) }

// ValidName reports whether name is a valid node name: letters, digits,
// dots and hyphens, starting and ending with a letter or digit.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// ValidPin reports whether pin has the form of a key pin: "sha256:" and 64
// lower-case hex digits.
func ValidPin(pin string) bool { return pinRE.MatchString(pin) }

// CheckAddress checks a member address: "ip:port" with one specific
// unicast IP address (no zone, not the unspecified, a multicast or a
// broadcast address), written in its canonical form, and a port that is
// not 0. The port is not fixed to 8444 here so that the multi-node tests
// can run on loopback; skep-cluster always writes 8444.
func CheckAddress(addr string) error {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return fmt.Errorf("address %q is not ip:port", addr)
	}
	a := ap.Addr()
	switch {
	case a.Zone() != "":
		return fmt.Errorf("address %q must not carry a zone", addr)
	case a.IsUnspecified(), a.IsMulticast(), a == netip.IPv4Unspecified(), a == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return fmt.Errorf("address %q is not one specific unicast address", addr)
	case a.Is4In6():
		return fmt.Errorf("address %q must be written as an IPv4 address", addr)
	case ap.Port() == 0:
		return fmt.Errorf("address %q has port 0", addr)
	case ap.String() != addr:
		return fmt.Errorf("address %q must be written as %q", addr, ap.String())
	}
	return nil
}
