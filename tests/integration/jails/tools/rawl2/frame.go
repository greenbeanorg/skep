//go:build skep_testtasks

// Command rawl2 writes hand-built Ethernet frames to an interface through
// bpf(4). It is the layer-2 measuring tool for the M7c anti-spoofing probe
// (tests/integration/jails/results/m7c-probe.md): ARP with any sender
// address in the payload, IPv4 ICMP and DHCP DISCOVER with any source MAC,
// and an ICMPv6 echo to ff02::1. A VNET jail's root can open /dev/bpf
// (step10-progress §31.1), so this is what a hostile jail can send. It is
// copied into a test jail by hand, built only with the skep_testtasks tag,
// and never part of a shipped binary.
//
// Usage:
//
//	rawl2 -if skep0 -mode arp  -eth-src MAC [-eth-dst MAC] -op request|reply -sha MAC -spa IP [-tha MAC] -tpa IP
//	rawl2 -if skep0 -mode icmp -eth-src MAC -eth-dst MAC -src IP -dst IP
//	rawl2 -if skep0 -mode dhcp -eth-src MAC
//	rawl2 -if skep0 -mode ipv6 -eth-src MAC
//	rawl2 -if skep0 -mode dhcpoffer -eth-src MAC -src IP   (UDP 67 -> 68: a rogue DHCP server)
//	rawl2 -if skep0 -mode ra -eth-src MAC                  (ICMPv6 router advertisement)
//	rawl2 -if skep0 -mode lldp -eth-src MAC
//	rawl2 -if skep0 -mode stp -eth-src MAC                 (802.3/LLC configuration BPDU)
//
// Every mode takes -count N (one frame per second) and -vlan VID[,VID...],
// which inserts 802.1Q tags (TPID 0x8100, outermost first) after the
// source MAC; two VIDs make a stacked (Q-in-Q style) frame.
package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
)

// Ethernet types.
const (
	etherIPv4 = 0x0800
	etherARP  = 0x0806
	etherIPv6 = 0x86dd
)

// minFrame is the shortest Ethernet frame without the FCS; shorter frames
// are padded with zeroes.
const minFrame = 60

var broadcast = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// ethernet returns an Ethernet header followed by payload, padded to
// minFrame.
func ethernet(dst, src net.HardwareAddr, etype uint16, payload []byte) ([]byte, error) {
	if len(dst) != 6 || len(src) != 6 {
		return nil, fmt.Errorf("rawl2: MAC addresses must be 6 bytes (dst %v, src %v)", dst, src)
	}
	n := 14 + len(payload)
	if n < minFrame {
		n = minFrame
	}
	b := make([]byte, n)
	copy(b[0:6], dst)
	copy(b[6:12], src)
	binary.BigEndian.PutUint16(b[12:], etype)
	copy(b[14:], payload)
	return b, nil
}

// arpPayload returns an Ethernet/IPv4 ARP packet (RFC 826). op is 1 for a
// request and 2 for a reply.
func arpPayload(op uint16, sha net.HardwareAddr, spa netip.Addr, tha net.HardwareAddr, tpa netip.Addr) ([]byte, error) {
	if len(sha) != 6 || len(tha) != 6 || !spa.Is4() || !tpa.Is4() {
		return nil, fmt.Errorf("rawl2: ARP needs 6-byte MACs and IPv4 addresses")
	}
	b := make([]byte, 28)
	binary.BigEndian.PutUint16(b[0:], 1) // hardware type Ethernet
	binary.BigEndian.PutUint16(b[2:], etherIPv4)
	b[4], b[5] = 6, 4
	binary.BigEndian.PutUint16(b[6:], op)
	s, t := spa.As4(), tpa.As4()
	copy(b[8:14], sha)
	copy(b[14:18], s[:])
	copy(b[18:24], tha)
	copy(b[24:28], t[:])
	return b, nil
}

// ipv4 returns an IPv4 header (no options) with its checksum, followed by
// payload.
func ipv4(src, dst netip.Addr, proto uint8, id uint16, payload []byte) ([]byte, error) {
	if !src.Is4() || !dst.Is4() {
		return nil, fmt.Errorf("rawl2: IPv4 addresses only (src %v, dst %v)", src, dst)
	}
	b := make([]byte, 20+len(payload))
	b[0] = 0x45
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)))
	binary.BigEndian.PutUint16(b[4:], id)
	b[8] = 64
	b[9] = proto
	s, d := src.As4(), dst.As4()
	copy(b[12:16], s[:])
	copy(b[16:20], d[:])
	binary.BigEndian.PutUint16(b[10:], checksum(b[:20]))
	copy(b[20:], payload)
	return b, nil
}

// icmpEcho returns an ICMP echo request with its checksum.
func icmpEcho(id, seq uint16) []byte {
	b := make([]byte, 8+16)
	b[0] = 8
	binary.BigEndian.PutUint16(b[4:], id)
	binary.BigEndian.PutUint16(b[6:], seq)
	copy(b[8:], "skep-m7c-rawl2..")
	binary.BigEndian.PutUint16(b[2:], checksum(b))
	return b
}

// dhcpDiscover returns a UDP datagram (68 -> 67, checksum 0, which IPv4
// allows) carrying a minimal BOOTP/DHCP DISCOVER for chaddr.
func dhcpDiscover(chaddr net.HardwareAddr, xid uint32) []byte {
	bootp := make([]byte, 240+4)
	bootp[0], bootp[1], bootp[2] = 1, 1, 6 // BOOTREQUEST, Ethernet, hlen 6
	binary.BigEndian.PutUint32(bootp[4:], xid)
	binary.BigEndian.PutUint16(bootp[10:], 0x8000) // broadcast flag
	copy(bootp[28:34], chaddr)
	copy(bootp[236:240], []byte{99, 130, 83, 99}) // magic cookie
	copy(bootp[240:], []byte{53, 1, 1, 255})      // DHCP DISCOVER, end
	udp := make([]byte, 8+len(bootp))
	binary.BigEndian.PutUint16(udp[0:], 68)
	binary.BigEndian.PutUint16(udp[2:], 67)
	binary.BigEndian.PutUint16(udp[4:], uint16(len(udp)))
	copy(udp[8:], bootp)
	return udp
}

// ipv6Echo returns an IPv6 header and an ICMPv6 echo request from src to
// dst, with the ICMPv6 checksum over the pseudo-header (RFC 8200 8.1).
func ipv6Echo(src, dst netip.Addr, id, seq uint16) ([]byte, error) {
	if !src.Is6() || src.Is4In6() || !dst.Is6() || dst.Is4In6() {
		return nil, fmt.Errorf("rawl2: IPv6 addresses only (src %v, dst %v)", src, dst)
	}
	ic := make([]byte, 8+16)
	ic[0] = 128
	binary.BigEndian.PutUint16(ic[4:], id)
	binary.BigEndian.PutUint16(ic[6:], seq)
	copy(ic[8:], "skep-m7c-rawl2..")
	s, d := src.As16(), dst.As16()
	pseudo := make([]byte, 0, 40+len(ic))
	pseudo = append(pseudo, s[:]...)
	pseudo = append(pseudo, d[:]...)
	pseudo = binary.BigEndian.AppendUint32(pseudo, uint32(len(ic)))
	pseudo = append(pseudo, 0, 0, 0, 58)
	pseudo = append(pseudo, ic...)
	binary.BigEndian.PutUint16(ic[2:], checksum(pseudo))

	b := make([]byte, 40+len(ic))
	b[0] = 0x60
	binary.BigEndian.PutUint16(b[4:], uint16(len(ic)))
	b[6] = 58 // ICMPv6
	b[7] = 255
	copy(b[8:24], s[:])
	copy(b[24:40], d[:])
	copy(b[40:], ic)
	return b, nil
}

// dhcpOffer returns a UDP datagram from port 67 to 68 (checksum 0)
// carrying a minimal BOOTREPLY/DHCP OFFER: what a guest acting as a DHCP
// server sends (ADR 0011 answer 5).
func dhcpOffer(chaddr net.HardwareAddr, xid uint32, yiaddr netip.Addr) []byte {
	bootp := make([]byte, 240+4)
	bootp[0], bootp[1], bootp[2] = 2, 1, 6 // BOOTREPLY, Ethernet, hlen 6
	binary.BigEndian.PutUint32(bootp[4:], xid)
	y := yiaddr.As4()
	copy(bootp[16:20], y[:])
	copy(bootp[28:34], chaddr)
	copy(bootp[236:240], []byte{99, 130, 83, 99})
	copy(bootp[240:], []byte{53, 1, 2, 255}) // DHCP OFFER, end
	udp := make([]byte, 8+len(bootp))
	binary.BigEndian.PutUint16(udp[0:], 67)
	binary.BigEndian.PutUint16(udp[2:], 68)
	binary.BigEndian.PutUint16(udp[4:], uint16(len(udp)))
	copy(udp[8:], bootp)
	return udp
}

// ipv6RA returns an IPv6 header and a minimal ICMPv6 router advertisement
// (type 134, router lifetime 1800 s) from src to ff02::1, with its
// checksum.
func ipv6RA(src netip.Addr) ([]byte, error) {
	dst := netip.MustParseAddr("ff02::1")
	if !src.Is6() || src.Is4In6() {
		return nil, fmt.Errorf("rawl2: IPv6 source only (%v)", src)
	}
	ic := make([]byte, 16)
	ic[0] = 134
	ic[4] = 64                               // cur hop limit
	binary.BigEndian.PutUint16(ic[6:], 1800) // router lifetime
	s, d := src.As16(), dst.As16()
	pseudo := make([]byte, 0, 40+len(ic))
	pseudo = append(pseudo, s[:]...)
	pseudo = append(pseudo, d[:]...)
	pseudo = binary.BigEndian.AppendUint32(pseudo, uint32(len(ic)))
	pseudo = append(pseudo, 0, 0, 0, 58)
	pseudo = append(pseudo, ic...)
	binary.BigEndian.PutUint16(ic[2:], checksum(pseudo))
	b := make([]byte, 40+len(ic))
	b[0] = 0x60
	binary.BigEndian.PutUint16(b[4:], uint16(len(ic)))
	b[6], b[7] = 58, 255
	copy(b[8:24], s[:])
	copy(b[24:40], d[:])
	copy(b[40:], ic)
	return b, nil
}

// stpBPDU returns an 802.3 frame (length field, LLC 42 42 03) carrying a
// configuration BPDU to the bridge group address, from src.
func stpBPDU(src net.HardwareAddr) []byte {
	bpdu := make([]byte, 35) // protocol 0, version 0, type 0 (config)
	copy(bpdu[5:11], src)    // root ID: priority 0 + our MAC, to win
	llc := append([]byte{0x42, 0x42, 0x03}, bpdu...)
	b := make([]byte, 14+len(llc))
	copy(b[0:6], []byte{0x01, 0x80, 0xc2, 0, 0, 0})
	copy(b[6:12], src)
	binary.BigEndian.PutUint16(b[12:], uint16(len(llc)))
	copy(b[14:], llc)
	for len(b) < minFrame {
		b = append(b, 0)
	}
	return b
}

// lldpFrame returns a minimal LLDP frame (chassis ID, port ID, TTL, end).
func lldpFrame(src net.HardwareAddr) ([]byte, error) {
	p := []byte{0x02, 0x07, 0x04}
	p = append(p, src...)                               // chassis ID: MAC
	p = append(p, 0x04, 0x05, 0x05, 's', 'k', 'e', 'p') // port ID: interface name
	p = append(p, 0x06, 0x02, 0x00, 0x78)               // TTL 120
	p = append(p, 0x00, 0x00)                           // end
	return ethernet(net.HardwareAddr{0x01, 0x80, 0xc2, 0, 0, 0x0e}, src, 0x88cc, p)
}

// tagVLAN returns frame with one 802.1Q tag (TPID 0x8100, PCP 0) per VID
// inserted after the source MAC, outermost first. A VID must be 1 to 4094.
func tagVLAN(frame []byte, vids []uint16) ([]byte, error) {
	if len(frame) < 14 {
		return nil, fmt.Errorf("rawl2: frame too short to tag (%d bytes)", len(frame))
	}
	out := make([]byte, 0, len(frame)+4*len(vids))
	out = append(out, frame[:12]...)
	for _, v := range vids {
		if v < 1 || v > 4094 {
			return nil, fmt.Errorf("rawl2: VLAN ID %d out of range 1-4094", v)
		}
		out = binary.BigEndian.AppendUint16(out, 0x8100)
		out = binary.BigEndian.AppendUint16(out, v)
	}
	return append(out, frame[12:]...), nil
}

// checksum is the Internet checksum (RFC 1071).
func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
