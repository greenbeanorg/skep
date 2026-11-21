//go:build skep_testtasks

package main

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
)

func mustMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	m, err := net.ParseMAC(s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestARPFrame(t *testing.T) {
	eth := mustMAC(t, "02:5e:00:0a:00:10")
	sha := mustMAC(t, "02:5e:00:00:00:01")
	p, err := arpPayload(2, sha, netip.MustParseAddr("192.0.2.20"), broadcast, netip.MustParseAddr("192.0.2.20"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := ethernet(broadcast, eth, etherARP, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != minFrame {
		t.Fatalf("len %d, want %d (padded)", len(f), minFrame)
	}
	if !bytes.Equal(f[6:12], eth) || binary.BigEndian.Uint16(f[12:]) != etherARP {
		t.Fatalf("ethernet header % x", f[:14])
	}
	a := f[14:]
	if binary.BigEndian.Uint16(a[6:]) != 2 || !bytes.Equal(a[8:14], sha) ||
		!bytes.Equal(a[14:18], []byte{192, 0, 2, 20}) || !bytes.Equal(a[24:28], []byte{192, 0, 2, 20}) {
		t.Fatalf("arp % x", a[:28])
	}
	// The sender MAC in the payload is independent of the Ethernet source.
	if bytes.Equal(a[8:14], f[6:12]) {
		t.Fatal("sha should differ from the ethernet source in this test")
	}
}

func TestIPv4Checksums(t *testing.T) {
	ip, err := ipv4(netip.MustParseAddr("0.0.0.0"), netip.MustParseAddr("255.255.255.255"), 17, 7,
		dhcpDiscover(mustMAC(t, "02:5e:00:0a:00:10"), 0x5302))
	if err != nil {
		t.Fatal(err)
	}
	if checksum(ip[:20]) != 0 {
		t.Fatal("IPv4 header checksum does not verify")
	}
	udp := ip[20:]
	if binary.BigEndian.Uint16(udp[0:]) != 68 || binary.BigEndian.Uint16(udp[2:]) != 67 ||
		int(binary.BigEndian.Uint16(udp[4:])) != len(udp) {
		t.Fatalf("udp header % x", udp[:8])
	}
	if udp[8+240] != 53 || udp[8+242] != 1 {
		t.Fatal("not a DHCP DISCOVER")
	}
	ic := icmpEcho(1, 2)
	if checksum(ic) != 0 {
		t.Fatal("ICMP checksum does not verify")
	}
	if _, err := ipv4(netip.MustParseAddr("::1"), netip.MustParseAddr("192.0.2.1"), 1, 0, nil); err == nil {
		t.Fatal("IPv6 source accepted")
	}
}

func TestIPv6EchoChecksum(t *testing.T) {
	src, dst := netip.MustParseAddr("fe80::5e:ff:fe0a:10"), netip.MustParseAddr("ff02::1")
	p, err := ipv6Echo(src, dst, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if p[0]>>4 != 6 || p[6] != 58 || int(binary.BigEndian.Uint16(p[4:])) != len(p)-40 {
		t.Fatalf("ipv6 header % x", p[:40])
	}
	ic := p[40:]
	s, d := src.As16(), dst.As16()
	pseudo := append(append(append([]byte{}, s[:]...), d[:]...), 0, 0, 0, byte(len(ic)), 0, 0, 0, 58)
	if checksum(append(pseudo, ic...)) != 0 {
		t.Fatal("ICMPv6 checksum does not verify")
	}
}
