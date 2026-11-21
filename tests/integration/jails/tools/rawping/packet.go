//go:build skep_testtasks

// Command rawping sends ICMP echo requests with a chosen IPv4 source
// address over a raw socket with IP_HDRINCL. It is the measuring tool for
// the SR-02 anti-spoofing tests (step10-progress §31): it is copied into a
// test jail by hand, it is built only with the skep_testtasks tag, and it
// is never part of a shipped binary.
//
// Usage: rawping -src A.B.C.D -dst A.B.C.D [-count N] [-ttl N] [-id N]
package main

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

// echoPacket returns an IPv4 header and an ICMP echo request, with both
// checksums filled in and every multi-byte field in network byte order
// (FreeBSD 11 and later take ip_len and ip_off that way with IP_HDRINCL).
func echoPacket(src, dst netip.Addr, ttl uint8, id, seq uint16, payload []byte) ([]byte, error) {
	if !src.Is4() || !dst.Is4() {
		return nil, fmt.Errorf("rawping: IPv4 addresses only (src %v, dst %v)", src, dst)
	}
	const ipLen, icmpLen = 20, 8
	total := ipLen + icmpLen + len(payload)
	if total > 65535 {
		return nil, fmt.Errorf("rawping: packet too large (%d bytes)", total)
	}
	b := make([]byte, total)
	b[0] = 0x45 // version 4, header length 5 words
	binary.BigEndian.PutUint16(b[2:], uint16(total))
	binary.BigEndian.PutUint16(b[4:], id)
	b[8] = ttl
	b[9] = 1 // ICMP
	s, d := src.As4(), dst.As4()
	copy(b[12:16], s[:])
	copy(b[16:20], d[:])
	binary.BigEndian.PutUint16(b[10:], checksum(b[:ipLen]))

	ic := b[ipLen:]
	ic[0] = 8 // echo request
	binary.BigEndian.PutUint16(ic[4:], id)
	binary.BigEndian.PutUint16(ic[6:], seq)
	copy(ic[icmpLen:], payload)
	binary.BigEndian.PutUint16(ic[2:], checksum(ic))
	return b, nil
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
