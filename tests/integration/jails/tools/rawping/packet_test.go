//go:build skep_testtasks

package main

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

func TestEchoPacket(t *testing.T) {
	src := netip.MustParseAddr("192.0.2.10")
	dst := netip.MustParseAddr("10.79.60.1")
	b, err := echoPacket(src, dst, 64, 0x1234, 7, []byte("skep"))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 32 {
		t.Fatalf("len = %d, want 32", len(b))
	}
	if got := binary.BigEndian.Uint16(b[2:]); got != 32 {
		t.Errorf("ip_len = %d, want 32 (network order)", got)
	}
	if b[9] != 1 || b[8] != 64 {
		t.Errorf("proto %d ttl %d", b[9], b[8])
	}
	if netip.AddrFrom4([4]byte(b[12:16])) != src || netip.AddrFrom4([4]byte(b[16:20])) != dst {
		t.Errorf("addresses %v -> %v", b[12:16], b[16:20])
	}
	// A correct checksum makes the checksum over the whole header zero.
	if c := checksum(b[:20]); c != 0 {
		t.Errorf("IP header checksum does not verify: %#04x", c)
	}
	if c := checksum(b[20:]); c != 0 {
		t.Errorf("ICMP checksum does not verify: %#04x", c)
	}
	if b[20] != 8 || binary.BigEndian.Uint16(b[26:]) != 7 {
		t.Errorf("ICMP type %d seq %d", b[20], binary.BigEndian.Uint16(b[26:]))
	}
}

func TestEchoPacketRejectsIPv6(t *testing.T) {
	if _, err := echoPacket(netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("10.79.60.1"), 64, 1, 1, nil); err == nil {
		t.Fatal("IPv6 source accepted")
	}
}

func TestChecksumOddLength(t *testing.T) {
	// RFC 1071 example bytes plus an odd trailing byte.
	b := []byte{0x00, 0x01, 0xf2, 0x03, 0xf4, 0xf5, 0xf6, 0xf7, 0x01}
	if got, want := checksum(b), ^uint16(0xddf2+0x0100); got != want {
		t.Errorf("checksum = %#04x, want %#04x", got, want)
	}
}
