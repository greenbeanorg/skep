package containers

import (
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
			mac, err := normalizeMAC(spec.MACAddress)
			if err != nil {
				return nil, nil, models.Invalid(field + "." + err.Error())
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
// unicast, not zero. It returns the lower-case colon form.
func normalizeMAC(s string) (string, error) {
	mac, err := net.ParseMAC(s)
	if err != nil || len(mac) != 6 {
		return "", fmt.Errorf("mac_address must be six bytes, such as 02:00:00:aa:bb:cc")
	}
	if mac[0]&1 != 0 {
		return "", fmt.Errorf("mac_address must be a unicast address")
	}
	if mac.String() == "00:00:00:00:00:00" {
		return "", fmt.Errorf("mac_address must not be all zeroes")
	}
	return mac.String(), nil
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
// line per NIC by its stable name, and the default route.
func netRCConf(c models.Container) ([]string, error) {
	var lines []string
	for _, n := range c.NICs {
		jif, err := hostops.JailInterfaceName(c.ID, n.Slot)
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
func jailNICs(c models.Container) []hostops.JailNIC {
	var out []hostops.JailNIC
	for _, n := range c.NICs {
		out = append(out, hostops.JailNIC{Slot: n.Slot, Bridge: n.Bridge, MAC: n.MACAddress, DHCP: n.IPv4.Mode == models.IPv4DHCP})
	}
	return out
}

// withInterfaces fills in the stable name of each NIC's jail end.
func withInterfaces(c models.Container) models.Container {
	if c.NICs == nil {
		c.NICs = []models.ContainerNIC{}
	}
	for i := range c.NICs {
		c.NICs[i].Interface, _ = hostops.JailInterfaceName(c.ID, c.NICs[i].Slot)
	}
	return c
}
