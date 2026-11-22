// Package portfilter generates the anti-spoofing filter of a guest port
// (ADR 0011): a classic BPF program for an ng_bpf(4) node hung between the
// host end's ng_ether(4) "lower" and "upper" hooks,
//
//	<if>:lower -> [hook "jail"]  ng_bpf  [hook "bridge"] -> <if>:upper
//
// and the ngctl(8) scripts that install and inspect it. Frames the guest
// sends arrive on "jail" and are checked by the program; only matches go
// on to "bridge" and from there into if_bridge. The "bridge" hook carries
// only frames the host stack sends out of the host end, which has no
// address, and passes them unchecked.
//
// The package only computes: programs, scripts, and the parsing of ngctl
// output. Running ngctl is internal/hostops' job.
package portfilter

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
)

// Classic BPF opcodes used here (sys/net/bpf.h).
const (
	opLdW   = 0x20 // BPF_LD|BPF_W|BPF_ABS
	opLdH   = 0x28 // BPF_LD|BPF_H|BPF_ABS
	opLdB   = 0x30 // BPF_LD|BPF_B|BPF_ABS
	opLdHI  = 0x48 // BPF_LD|BPF_H|BPF_IND
	opLdxMS = 0xb1 // BPF_LDX|BPF_MSH|BPF_B: X = 4*(P[k]&0xf)
	opAndK  = 0x54 // BPF_ALU|BPF_AND|BPF_K
	opJeqK  = 0x15 // BPF_JMP|BPF_JEQ|BPF_K
	opJgeK  = 0x35 // BPF_JMP|BPF_JGE|BPF_K
	opJsetK = 0x45 // BPF_JMP|BPF_JSET|BPF_K
	opRetK  = 0x06 // BPF_RET|BPF_K
)

// Accept is what the program returns for a frame that may pass. ng_bpf
// truncates a matching frame to the returned length, so it must be at
// least any frame's length.
const Accept = 0xffffffff

// MaxInsns is FreeBSD's default net.bpf.maxinsns; a program must stay
// below it.
const MaxInsns = 512

// Ethertypes and protocol numbers the program looks at.
const (
	etherIPv4 = 0x0800
	etherARP  = 0x0806
	etherIPv6 = 0x86dd

	protoUDP    = 17
	protoICMPv6 = 58

	dhcpServerPort = 67
	icmpv6RA       = 134
)

// Insn is one classic BPF instruction (struct bpf_insn).
type Insn struct {
	Code   uint16
	Jt, Jf uint8
	K      uint32
}

// Params is what a port's program is generated from: the NIC as the API
// recorded it.
type Params struct {
	// MAC is the guest's recorded MAC address: unicast, six bytes.
	MAC net.HardwareAddr
	// IPv4 is the configured address of a static NIC. The zero Addr
	// means MAC-only: a DHCP jail NIC or a VM NIC (Skep records no
	// address for a VM).
	IPv4 netip.Addr
	// IPv6 allows ethertype 0x86dd, MAC-only, with the router
	// advertisement guard: a jail NIC with ipv6_mode slaac or static,
	// and every VM NIC (ADR 0011 answer 4). Without it IPv6 is dropped.
	IPv6 bool
}

// Static reports whether the program pins the IPv4 source.
func (p Params) Static() bool { return p.IPv4.IsValid() }

// Validate checks p.
func (p Params) Validate() error {
	if len(p.MAC) != 6 {
		return fmt.Errorf("portfilter: the MAC address must be six bytes, got %d", len(p.MAC))
	}
	if p.MAC[0]&1 != 0 {
		return fmt.Errorf("portfilter: %s is a multicast MAC address", p.MAC)
	}
	if p.IPv4.IsValid() && (!p.IPv4.Is4() || p.IPv4.IsUnspecified() || p.IPv4.IsMulticast() || p.IPv4 == netip.AddrFrom4([4]byte{255, 255, 255, 255})) {
		return fmt.Errorf("portfilter: %s is not a unicast IPv4 address", p.IPv4)
	}
	return nil
}

// asm is a tiny assembler with symbolic forward jumps, so the program
// reads top to bottom and no offset is counted by hand.
type asm struct {
	insns  []Insn
	labels map[string]int
	fixups []fixup
}

type fixup struct {
	at     int
	jt, jf string
}

func (a *asm) op(code uint16, k uint32) { a.insns = append(a.insns, Insn{Code: code, K: k}) }

// jmp emits a conditional jump to label t when true, f when false; an
// empty label is the next instruction.
func (a *asm) jmp(code uint16, k uint32, t, f string) {
	a.fixups = append(a.fixups, fixup{at: len(a.insns), jt: t, jf: f})
	a.insns = append(a.insns, Insn{Code: code, K: k})
}

func (a *asm) label(name string) {
	if a.labels == nil {
		a.labels = map[string]int{}
	}
	a.labels[name] = len(a.insns)
}

func (a *asm) link() ([]Insn, error) {
	off := func(at int, l string) (uint8, error) {
		if l == "" {
			return 0, nil
		}
		to, ok := a.labels[l]
		if !ok {
			return 0, fmt.Errorf("portfilter: undefined label %q", l)
		}
		d := to - at - 1
		if d < 0 || d > 255 {
			return 0, fmt.Errorf("portfilter: jump to %q out of range (%d)", l, d)
		}
		return uint8(d), nil
	}
	for _, f := range a.fixups {
		var err error
		if a.insns[f.at].Jt, err = off(f.at, f.jt); err != nil {
			return nil, err
		}
		if a.insns[f.at].Jf, err = off(f.at, f.jf); err != nil {
			return nil, err
		}
	}
	if len(a.insns) >= MaxInsns {
		return nil, errors.New("portfilter: the program is too long")
	}
	return a.insns, nil
}

// Program returns the filter for frames the guest sends (the "jail" hook).
// It is default deny:
//
//   - the Ethernet source must be the recorded MAC (requirement A);
//   - IPv4 (0x0800) must be version 4 with a header length of at least 20
//     bytes, completely present; a static NIC's source must be the
//     configured address (B). UDP from source port 67 (a DHCP server) is
//     dropped on every port, found through the header length and checked
//     in a datagram's first fragment only (later fragments carry no UDP
//     header and pass on; without their first fragment they never
//     reassemble);
//   - ARP (0x0806) must be Ethernet/IPv4 with the recorded sender MAC and,
//     on a static NIC, the configured sender IP or 0.0.0.0, the RFC 5227
//     probe (C);
//   - IPv6 (0x86dd), when allowed, passes MAC-only, except an ICMPv6
//     router advertisement directly after the IPv6 header (the RA guard;
//     best-effort: one behind an extension header is not recognised);
//     otherwise IPv6 is dropped (F);
//   - everything else is dropped: STP and other 802.3 (LLC) frames, LLDP,
//     slow protocols, an 802.1Q tag left in the frame data (Q-in-Q).
//
// Offsets are from the start of the Ethernet frame as ng_ether hands it
// over; a single 802.1Q tag is already out of the frame data (M_VLANTAG),
// which the bridge's VLAN filtering covers. A load past the end of the
// frame makes bpf_filter() return 0, so short frames are dropped.
func Program(p Params) ([]Insn, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	macHi := binary.BigEndian.Uint32(p.MAC[0:4])
	macLo := uint32(binary.BigEndian.Uint16(p.MAC[4:6]))
	var ip32 uint32
	if p.Static() {
		b := p.IPv4.As4()
		ip32 = binary.BigEndian.Uint32(b[:])
	}

	a := &asm{}
	// Ethernet source MAC (bytes 6-11).
	a.op(opLdW, 6)
	a.jmp(opJeqK, macHi, "", "drop")
	a.op(opLdH, 10)
	a.jmp(opJeqK, macLo, "", "drop")
	// Ethertype (bytes 12-13).
	a.op(opLdH, 12)
	a.jmp(opJeqK, etherIPv4, "ipv4", "")
	if p.IPv6 {
		a.jmp(opJeqK, etherARP, "arp", "")
		a.jmp(opJeqK, etherIPv6, "ipv6", "drop")
	} else {
		a.jmp(opJeqK, etherARP, "arp", "drop")
	}

	a.label("ipv4")
	// Version 4 and IHL >= 5 (byte 14).
	a.op(opLdB, 14)
	a.op(opAndK, 0xf0)
	a.jmp(opJeqK, 0x40, "", "drop")
	a.op(opLdB, 14)
	a.op(opAndK, 0x0f)
	a.jmp(opJgeK, 5, "", "drop")
	// The last word of a minimal header (the destination, 14 + 16): the
	// value is not used, the load drops a frame too short for a header.
	a.op(opLdW, 30)
	if p.Static() {
		a.op(opLdW, 26) // source address, 14 + 12
		a.jmp(opJeqK, ip32, "", "drop")
	}
	// The DHCP-server guard.
	a.op(opLdB, 23) // protocol, 14 + 9
	a.jmp(opJeqK, protoUDP, "", "accept")
	a.op(opLdH, 20) // flags and fragment offset, 14 + 6
	a.jmp(opJsetK, 0x1fff, "accept", "")
	a.op(opLdxMS, 14) // X = header length in bytes
	a.op(opLdHI, 14)  // UDP source port at 14 + X
	a.jmp(opJeqK, dhcpServerPort, "drop", "accept")

	a.label("arp")
	a.op(opLdW, 14) // htype 1 (Ethernet), ptype 0x0800
	a.jmp(opJeqK, 0x00010800, "", "drop")
	a.op(opLdH, 18) // hlen 6, plen 4
	a.jmp(opJeqK, 0x0604, "", "drop")
	a.op(opLdW, 22) // sender MAC: 14 + 8
	a.jmp(opJeqK, macHi, "", "drop")
	a.op(opLdH, 26)
	if p.Static() {
		a.jmp(opJeqK, macLo, "", "drop")
		a.op(opLdW, 28) // sender IP: 14 + 14
		a.jmp(opJeqK, ip32, "accept", "")
		a.jmp(opJeqK, 0, "accept", "drop") // RFC 5227 probe
	} else {
		a.jmp(opJeqK, macLo, "accept", "drop")
	}

	if p.IPv6 {
		a.label("ipv6")
		a.op(opLdB, 20) // next header, 14 + 6
		a.jmp(opJeqK, protoICMPv6, "", "accept")
		a.op(opLdB, 54) // ICMPv6 type, 14 + 40
		a.jmp(opJeqK, icmpv6RA, "drop", "accept")
	}

	a.label("accept")
	a.op(opRetK, Accept)
	a.label("drop")
	a.op(opRetK, 0)
	return a.link()
}

// PassAll is the program of the "bridge" hook: every frame matches.
var PassAll = []Insn{{Code: opRetK, K: Accept}}

// Equal reports whether two programs are the same, instruction by
// instruction.
func Equal(a, b []Insn) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
