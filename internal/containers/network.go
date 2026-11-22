package containers

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"net/netip"
	"reflect"

	"github.com/greenbeanorg/skep/internal/config"
	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/ids"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Jail networking (ADR 0010 decision 10, amendment h): the NICs a request
// asks for, checked and turned into the jail's rc.conf and resolv.conf
// and into the epairs StartJail creates.

// normalizeNICs checks the NICs and nameservers of a request. old are the
// container's current NICs: a slot keeps its ID, and its MAC when the
// request gives none.
func (s *Service) normalizeNICs(specs []models.ContainerNICSpec, nameservers []string, old []models.ContainerNIC) ([]models.ContainerNIC, []string, error) {
	if len(specs) > models.MaxContainerNICs {
		return nil, nil, models.Invalid(fmt.Sprintf("a jail has at most %d NICs", models.MaxContainerNICs))
	}
	nics := []models.ContainerNIC{}
	gateways, dhcp := 0, false
	for slot, spec := range specs {
		field := fmt.Sprintf("nics[%d]", slot)
		if !config.ValidInterfaceName(spec.Bridge) {
			return nil, nil, models.Invalid(field + ".bridge must be the name of a bridge, such as bridge0")
		}
		if err := s.conf.CheckBridge(spec.Bridge); err != nil {
			return nil, nil, err
		}
		nic := models.ContainerNIC{Slot: slot, Bridge: spec.Bridge, IPv4: models.ContainerIPv4{Mode: models.IPv4DHCP}}
		if slot < len(old) {
			nic.ID = old[slot].ID
		}
		if nic.ID == "" {
			id, err := ids.NewUUID()
			if err != nil {
				return nil, nil, err
			}
			nic.ID = id
		}
		switch {
		case spec.MACAddress != "":
			mac, err := normalizeMAC(field, spec.MACAddress)
			if err != nil {
				return nil, nil, err
			}
			nic.MACAddress = mac
		case slot < len(old):
			nic.MACAddress = old[slot].MACAddress
		default:
			mac, err := newMAC()
			if err != nil {
				return nil, nil, err
			}
			nic.MACAddress = mac
		}
		if spec.IPv4 != nil {
			v4, err := normalizeIPv4(field, *spec.IPv4)
			if err != nil {
				return nil, nil, err
			}
			nic.IPv4 = v4
		}
		if nic.IPv4.Mode == models.IPv4DHCP {
			dhcp = true
		}
		if nic.IPv4.Gateway != "" {
			gateways++
		}
		nics = append(nics, nic)
	}
	seen := map[string]bool{}
	for _, n := range nics {
		if seen[n.MACAddress] {
			return nil, nil, models.Invalid("two NICs have the MAC address " + n.MACAddress)
		}
		seen[n.MACAddress] = true
	}
	if gateways > 1 {
		return nil, nil, models.Invalid("only one NIC may have a gateway: it is the jail's default route")
	}
	if len(nameservers) > 0 && dhcp {
		return nil, nil, models.Invalid("nameservers are for static addressing only: with DHCP, dhclient writes the jail's resolv.conf")
	}
	if len(nameservers) > models.MaxContainerNameservers {
		return nil, nil, models.Invalid(fmt.Sprintf("at most %d nameservers", models.MaxContainerNameservers))
	}
	var ns []string
	for i, v := range nameservers {
		a, err := netip.ParseAddr(v)
		if err != nil || a.Zone() != "" || a.IsUnspecified() || a.IsMulticast() {
			return nil, nil, models.Invalid(fmt.Sprintf("nameservers[%d] must be an IPv4 or IPv6 address", i))
		}
		ns = append(ns, a.String())
	}
	return nics, ns, nil
}

// normalizeIPv4 checks one NIC's IPv4 addressing.
func normalizeIPv4(field string, v models.ContainerIPv4) (models.ContainerIPv4, error) {
	switch v.Mode {
	case "", models.IPv4DHCP:
		if v.Address != "" || v.Gateway != "" {
			return v, models.Invalid(field + ".ipv4: address and gateway are for static addressing only")
		}
		return models.ContainerIPv4{Mode: models.IPv4DHCP}, nil
	case models.IPv4Static:
	default:
		return v, models.Invalid(field + `.ipv4.mode must be "dhcp" or "static"`)
	}
	p, err := netip.ParsePrefix(v.Address)
	if err != nil || !p.Addr().Is4() || p.Bits() < 1 {
		return v, models.Invalid(field + ".ipv4.address must be an IPv4 address with its prefix length, such as 192.0.2.10/24")
	}
	a := p.Addr()
	if a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() || a.IsLinkLocalUnicast() || a == netip.MustParseAddr("255.255.255.255") {
		return v, models.Invalid(field + ".ipv4.address must be a unicast address")
	}
	if p.Bits() <= 30 {
		if a == p.Masked().Addr() || a == lastAddr(p) {
			return v, models.Invalid(field + ".ipv4.address is the network or broadcast address of its prefix")
		}
	}
	out := models.ContainerIPv4{Mode: models.IPv4Static, Address: p.String()}
	if v.Gateway != "" {
		g, err := netip.ParseAddr(v.Gateway)
		if err != nil || !g.Is4() || g.Zone() != "" {
			return v, models.Invalid(field + ".ipv4.gateway must be an IPv4 address")
		}
		if !p.Masked().Contains(g) || g == a {
			return v, models.Invalid(field + ".ipv4.gateway must be another address in the NIC's prefix")
		}
		out.Gateway = g.String()
	}
	return out, nil
}

// lastAddr is the broadcast address of an IPv4 prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v |= host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// normalizeMAC checks a MAC address a request supplied: six bytes,
// unicast, locally administered, and not zero (amendment n, SR-03). It
// returns the lower-case colon form. A malformed address is
// invalid_request; one that breaks a rule is container_mac_invalid (422).
func normalizeMAC(field, s string) (string, error) {
	mac, err := net.ParseMAC(s)
	if err != nil || len(mac) != 6 {
		return "", models.Invalid(field + ".mac_address must be six bytes, such as 02:00:00:aa:bb:cc")
	}
	if p := hostops.JailMACProblem(mac); p != "" {
		return "", &models.Error{Code: models.CodeContainerMACInvalid,
			Message: field + ".mac_address " + p,
			Details: map[string]any{"field": field + ".mac_address", "mac_address": mac.String()}}
	}
	return mac.String(), nil
}

// checkMACs refuses a start when a NIC's recorded MAC address breaks the
// rules (one stored before amendment n) or another Skep jail or VM holds
// it (amendment n, SR-03). The host side (the bridge, its members, and
// its address table) is checked by StartJail.
func (s *Service) checkMACs(ctx context.Context, c models.Container) error {
	for _, n := range c.NICs {
		field := fmt.Sprintf("nics[%d]", n.Slot)
		if _, err := normalizeMAC(field, n.MACAddress); err != nil {
			return err
		}
		if err := s.macHeld(ctx, c.ID, n); err != nil {
			return err
		}
	}
	return nil
}

// checkMACsFree refuses a create, an update, or a clone whose NIC has a
// MAC address another Skep jail or VM holds, with the start's
// container_mac_in_use (409) and its held_by details, before anything is
// stored (amendment u, finding 2). containerID is "" for a new jail. The
// store refuses the same again when it writes the NICs.
func (s *Service) checkMACsFree(ctx context.Context, containerID string, nics []models.ContainerNIC) error {
	for _, n := range nics {
		if err := s.macHeld(ctx, containerID, n); err != nil {
			return err
		}
	}
	return nil
}

// macHeld is container_mac_in_use when a Skep jail other than containerID,
// or a Skep VM, holds the NIC's MAC address.
func (s *Service) macHeld(ctx context.Context, containerID string, n models.ContainerNIC) error {
	holders, err := s.store.MACHolders(ctx, n.MACAddress, containerID)
	if err != nil {
		return err
	}
	if len(holders) == 0 {
		return nil
	}
	h := holders[0]
	details := map[string]any{"slot": n.Slot, "mac_address": n.MACAddress,
		"held_by_kind": h.Kind, "held_by_id": h.ID, "held_by_name": h.Name}
	if containerID != "" {
		details["container_id"] = containerID
	}
	return &models.Error{Code: models.CodeContainerMACInUse,
		Message: fmt.Sprintf("the MAC address %s of NIC slot %d is also held by the %s %s (slot %d); give the NIC another address",
			n.MACAddress, n.Slot, h.Kind, h.Name, h.Slot),
		Details: details}
}

// newMAC generates a locally administered unicast address: 02 and five
// random bytes, as for VMs.
func newMAC() (string, error) {
	b := make(net.HardwareAddr, 6)
	if _, err := rand.Read(b[1:]); err != nil {
		return "", err
	}
	b[0] = 0x02
	return b.String(), nil
}

// sameAddressing reports whether two NIC lists differ only in bridges and
// MACs: the change allowed after the first start, since the slots and
// addressing are in the jail's rc.conf.
func sameAddressing(a, b []models.ContainerNIC) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Slot != b[i].Slot || !reflect.DeepEqual(a[i].IPv4, b[i].IPv4) {
			return false
		}
	}
	return true
}

// netRCConf is what the jail's rc.conf needs for its NICs: one ifconfig_
// line per NIC by its guest name skep<slot> (amendment k), and the default
// route.
func netRCConf(c models.Container) ([]string, error) {
	var lines []string
	for _, n := range c.NICs {
		jif, err := hostops.GuestInterfaceName(n.Slot)
		if err != nil {
			return nil, err
		}
		if n.IPv4.Mode == models.IPv4Static {
			lines = append(lines, fmt.Sprintf(`ifconfig_%s="inet %s"`, jif, n.IPv4.Address))
			if n.IPv4.Gateway != "" {
				lines = append(lines, fmt.Sprintf(`defaultrouter="%s"`, n.IPv4.Gateway))
			}
			continue
		}
		lines = append(lines, fmt.Sprintf(`ifconfig_%s="SYNCDHCP"`, jif))
	}
	return lines, nil
}

// jailNICs is what StartJail needs of a container's NICs.
func jailNICs(c models.Container) []hostops.JailNIC { return JailNICs(c) }

// JailNICs is what StartJail, and the port-filter check, need of a
// container's NICs.
func JailNICs(c models.Container) []hostops.JailNIC {
	var out []hostops.JailNIC
	for _, n := range c.NICs {
		nic := hostops.JailNIC{Slot: n.Slot, Bridge: n.Bridge, MAC: n.MACAddress, DHCP: n.IPv4.Mode == models.IPv4DHCP}
		if !nic.DHCP {
			nic.IPv4 = n.IPv4.Address
		}
		// IPv6 stays false: the API offers no IPv6 setting yet, so every
		// jail NIC has ipv6_mode none and its port drops IPv6 (ADR 0011
		// decision 2).
		out = append(out, nic)
	}
	return out
}

// withInterfaces fills in the name of each NIC's jail end inside the jail.
func withInterfaces(c models.Container) models.Container {
	if c.NICs == nil {
		c.NICs = []models.ContainerNIC{}
	}
	for i := range c.NICs {
		c.NICs[i].Interface, _ = hostops.GuestInterfaceName(c.NICs[i].Slot)
	}
	return c
}

// sameIP reports whether two addresses in CIDR form have the same IP,
// whatever their prefix lengths.
func sameIP(a, b string) bool {
	pa, errA := netip.ParsePrefix(a)
	pb, errB := netip.ParsePrefix(b)
	return errA == nil && errB == nil && pa.Addr() == pb.Addr()
}

// checkAddressFree refuses to start a jail when a running Skep jail on the
// same bridge holds one of its static IPv4 addresses (amendment k). The
// addresses are the ones Skep wrote into each jail's rc.conf; an address
// changed inside a jail is not seen.
func (s *Service) checkAddressFree(ctx context.Context, c models.Container) error {
	var mine []models.ContainerNIC
	for _, n := range c.NICs {
		if n.IPv4.Mode == models.IPv4Static {
			mine = append(mine, n)
		}
	}
	if len(mine) == 0 {
		return nil
	}
	running, err := s.jails.RunningJails(ctx)
	if err != nil {
		return err
	}
	others, err := s.store.StaticNICs(ctx)
	if err != nil {
		return err
	}
	for _, n := range mine {
		for _, o := range others {
			if _, up := running[o.ContainerID]; !up || o.ContainerID == c.ID || o.Bridge != n.Bridge || !sameIP(o.Address, n.IPv4.Address) {
				continue
			}
			return &models.Error{Code: models.CodeContainerAddressInUse,
				Message: fmt.Sprintf("the running jail %s holds %s on %s; stop it or give this jail another address",
					o.ContainerName, o.Address, o.Bridge),
				Details: map[string]any{"slot": n.Slot, "address": n.IPv4.Address, "bridge": n.Bridge,
					"held_by_container_id": o.ContainerID, "held_by_container_name": o.ContainerName}}
		}
	}
	return nil
}
