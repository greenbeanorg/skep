package portfilter

import (
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
)

// run interprets p the way sys/net/bpf_filter.c does for the opcodes
// Program uses: any load past the end of the packet makes the whole
// filter return 0.
func run(t *testing.T, p []Insn, pkt []byte) uint32 {
	t.Helper()
	var a, x uint32
	for pc := 0; pc < len(p); pc++ {
		in := p[pc]
		k := int(in.K)
		switch in.Code {
		case opLdW:
			if k+4 > len(pkt) {
				return 0
			}
			a = binary.BigEndian.Uint32(pkt[k:])
		case opLdH:
			if k+2 > len(pkt) {
				return 0
			}
			a = uint32(binary.BigEndian.Uint16(pkt[k:]))
		case opLdB:
			if k >= len(pkt) {
				return 0
			}
			a = uint32(pkt[k])
		case opLdHI:
			at := int(x) + k
			if at+2 > len(pkt) {
				return 0
			}
			a = uint32(binary.BigEndian.Uint16(pkt[at:]))
		case opLdxMS:
			if k >= len(pkt) {
				return 0
			}
			x = uint32(pkt[k]&0xf) << 2
		case opAndK:
			a &= in.K
		case opJeqK, opJgeK, opJsetK:
			var cond bool
			switch in.Code {
			case opJeqK:
				cond = a == in.K
			case opJgeK:
				cond = a >= in.K
			default:
				cond = a&in.K != 0
			}
			if cond {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case opRetK:
			return in.K
		default:
			t.Fatalf("opcode 0x%x not emulated", in.Code)
		}
	}
	t.Fatal("fell off the end of the program")
	return 0
}

var (
	recMAC    = net.HardwareAddr{0x02, 0x5e, 0x00, 0x0a, 0x00, 0x10}
	forgedMAC = net.HardwareAddr{0x02, 0x5e, 0x00, 0x00, 0x00, 0x01}
	// nearMAC differs from recMAC only in the last byte, to catch a
	// program that checks only the first four.
	nearMAC  = net.HardwareAddr{0x02, 0x5e, 0x00, 0x0a, 0x00, 0x11}
	bcast    = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	stpDst   = net.HardwareAddr{0x01, 0x80, 0xc2, 0x00, 0x00, 0x00}
	recIP    = netip.MustParseAddr("192.0.2.10")
	otherIP  = netip.MustParseAddr("192.0.2.50")
	victimIP = netip.MustParseAddr("192.0.2.20")
	zeroIP   = netip.IPv4Unspecified()
)

func ethTo(dst, src net.HardwareAddr, etype uint16, payload []byte) []byte {
	b := make([]byte, 14, 60)
	copy(b[0:6], dst)
	copy(b[6:12], src)
	binary.BigEndian.PutUint16(b[12:], etype)
	b = append(b, payload...)
	for len(b) < 60 {
		b = append(b, 0)
	}
	return b
}

func eth(src net.HardwareAddr, etype uint16, payload []byte) []byte {
	return ethTo(bcast, src, etype, payload)
}

// ip4opts is an IPv4 header with the given source, protocol, header
// length in words, flags/fragment field, followed by payload.
func ip4opts(src netip.Addr, proto byte, ihl int, frag uint16, payload []byte) []byte {
	b := make([]byte, ihl*4)
	b[0] = 0x40 | byte(ihl)
	b[9] = proto
	binary.BigEndian.PutUint16(b[6:], frag)
	s, d := src.As4(), victimIP.As4()
	copy(b[12:], s[:])
	copy(b[16:], d[:])
	return append(b, payload...)
}

// ip4 is an ICMP packet from src.
func ip4(src netip.Addr) []byte { return ip4opts(src, 1, 5, 0, make([]byte, 8)) }

// udp is a UDP header with the given ports and a little payload.
func udp(sport, dport uint16) []byte {
	b := make([]byte, 16)
	binary.BigEndian.PutUint16(b[0:], sport)
	binary.BigEndian.PutUint16(b[2:], dport)
	return b
}

func arp(sha net.HardwareAddr, spa netip.Addr) []byte {
	b := make([]byte, 28)
	binary.BigEndian.PutUint16(b[0:], 1)
	binary.BigEndian.PutUint16(b[2:], 0x0800)
	b[4], b[5] = 6, 4
	binary.BigEndian.PutUint16(b[6:], 2)
	s, d := spa.As4(), victimIP.As4()
	copy(b[8:], sha)
	copy(b[14:], s[:])
	copy(b[24:], d[:])
	return b
}

// ip6 is an IPv6 header with next header nh, followed by payload.
func ip6(nh byte, payload []byte) []byte {
	b := make([]byte, 40)
	b[0] = 0x60
	binary.BigEndian.PutUint16(b[4:], uint16(len(payload)))
	b[6] = nh
	b[7] = 255
	return append(b, payload...)
}

func icmp6(typ byte) []byte { return []byte{typ, 0, 0, 0, 0, 0, 0, 0} }

func vlan(f []byte, vid uint16) []byte {
	out := append([]byte{}, f[:12]...)
	out = binary.BigEndian.AppendUint16(out, 0x8100)
	out = binary.BigEndian.AppendUint16(out, vid)
	return append(out, f[12:]...)
}

// The four kinds of port (ADR 0011 Decision 2 and answer 4).
var modes = []struct {
	name string
	p    Params
}{
	{"static", Params{MAC: recMAC, IPv4: recIP}},
	{"dhcp", Params{MAC: recMAC}},
	{"static+ipv6", Params{MAC: recMAC, IPv4: recIP, IPv6: true}},
	{"vm", Params{MAC: recMAC, IPv6: true}}, // MAC-only with IPv6: every VM port, a DHCP jail with IPv6
}

func TestProgramModes(t *testing.T) {
	badARPHlen := arp(recMAC, recIP)
	badARPHlen[4] = 8
	v6 := func(nh byte, payload []byte) []byte { return eth(recMAC, 0x86dd, ip6(nh, payload)) }
	hbhThenRA := append([]byte{58, 0, 1, 4, 0, 0, 0, 0}, icmp6(134)...) // hop-by-hop, then ICMPv6 RA

	// want: pass in static, dhcp, static+ipv6, vm.
	cases := []struct {
		name  string
		frame []byte
		want  [4]bool
	}{
		{"D ipv4 ok", eth(recMAC, 0x0800, ip4(recIP)), [4]bool{true, true, true, true}},
		{"D ipv4 with options", eth(recMAC, 0x0800, ip4opts(recIP, 1, 6, 0, make([]byte, 8))), [4]bool{true, true, true, true}},
		{"A forged eth src", eth(forgedMAC, 0x0800, ip4(recIP)), [4]bool{}},
		{"A near eth src (last byte)", eth(nearMAC, 0x0800, ip4(recIP)), [4]bool{}},
		{"B forged ipv4 src", eth(recMAC, 0x0800, ip4(otherIP)), [4]bool{false, true, false, true}},
		{"E dhcp discover from 0.0.0.0:68", eth(recMAC, 0x0800, ip4opts(zeroIP, 17, 5, 0, udp(68, 67))), [4]bool{false, true, false, true}},
		{"E dhcp from forged MAC", eth(forgedMAC, 0x0800, ip4opts(zeroIP, 17, 5, 0, udp(68, 67))), [4]bool{}},
		{"D udp to port 67 from the guest's address", eth(recMAC, 0x0800, ip4opts(recIP, 17, 5, 0, udp(68, 67))), [4]bool{true, true, true, true}},
		{"guard: udp source port 67", eth(recMAC, 0x0800, ip4opts(recIP, 17, 5, 0, udp(67, 68))), [4]bool{}},
		{"guard: udp source port 67 behind ip options", eth(recMAC, 0x0800, ip4opts(recIP, 17, 7, 0, udp(67, 68))), [4]bool{}},
		{"guard: udp 67 in a first fragment (MF set)", eth(recMAC, 0x0800, ip4opts(recIP, 17, 5, 0x2000, udp(67, 68))), [4]bool{}},
		{"guard: later fragment passes on", eth(recMAC, 0x0800, ip4opts(recIP, 17, 5, 0x2001, udp(67, 68))), [4]bool{true, true, true, true}},
		{"guard: later fragment from a forged source", eth(recMAC, 0x0800, ip4opts(otherIP, 17, 5, 0x0005, udp(67, 68))), [4]bool{false, true, false, true}},
		{"guard: udp header past the frame end", eth(recMAC, 0x0800, ip4opts(recIP, 17, 15, 0, nil))[:14+60], [4]bool{}},
		{"tcp source port 67 is not a DHCP server", eth(recMAC, 0x0800, ip4opts(recIP, 6, 5, 0, udp(67, 68))), [4]bool{true, true, true, true}},
		{"ipv4 header length below 5", eth(recMAC, 0x0800, ip4opts(recIP, 1, 4, 0, make([]byte, 12))), [4]bool{}},
		{"ipv4 version 6 in an 0x0800 frame", func() []byte {
			f := eth(recMAC, 0x0800, ip4(recIP))
			f[14] = 0x65
			return f
		}(), [4]bool{}},
		{"arp ok", eth(recMAC, 0x0806, arp(recMAC, recIP)), [4]bool{true, true, true, true}},
		{"arp probe 0.0.0.0", eth(recMAC, 0x0806, arp(recMAC, zeroIP)), [4]bool{true, true, true, true}},
		{"C1 arp forged sender ip", eth(recMAC, 0x0806, arp(recMAC, otherIP)), [4]bool{false, true, false, true}},
		{"C2 arp forged sender mac", eth(recMAC, 0x0806, arp(forgedMAC, recIP)), [4]bool{}},
		{"C2 arp near sender mac", eth(recMAC, 0x0806, arp(nearMAC, recIP)), [4]bool{}},
		{"C4 arp claims victim ip", eth(recMAC, 0x0806, arp(recMAC, victimIP)), [4]bool{false, true, false, true}},
		{"arp bad hlen", eth(recMAC, 0x0806, badARPHlen), [4]bool{}},
		{"arp forged eth src", eth(forgedMAC, 0x0806, arp(recMAC, recIP)), [4]bool{}},
		{"F/ipv6 tcp", v6(6, make([]byte, 20)), [4]bool{false, false, true, true}},
		{"ipv6 neighbour solicitation", v6(58, icmp6(135)), [4]bool{false, false, true, true}},
		{"ipv6 router solicitation", v6(58, icmp6(133)), [4]bool{false, false, true, true}},
		{"guard: ipv6 router advertisement", v6(58, icmp6(134)), [4]bool{}},
		{"guard (best-effort): RA behind hop-by-hop passes", v6(0, hbhThenRA), [4]bool{false, false, true, true}},
		{"ipv6 from a forged MAC", eth(forgedMAC, 0x86dd, ip6(6, make([]byte, 20))), [4]bool{}},
		{"vlan tag in data (Q-in-Q)", vlan(eth(recMAC, 0x0800, ip4(recIP)), 5), [4]bool{}},
		{"802.1ad ethertype", eth(recMAC, 0x88a8, make([]byte, 40)), [4]bool{}},
		{"(c) LLDP", ethTo(net.HardwareAddr{0x01, 0x80, 0xc2, 0, 0, 0x0e}, recMAC, 0x88cc, make([]byte, 40)), [4]bool{}},
		{"(c) STP BPDU (802.3 length field)", ethTo(stpDst, recMAC, 39, append([]byte{0x42, 0x42, 0x03}, make([]byte, 36)...)), [4]bool{}},
		{"(c) slow protocols (LACP)", ethTo(net.HardwareAddr{0x01, 0x80, 0xc2, 0, 0, 2}, recMAC, 0x8809, make([]byte, 40)), [4]bool{}},
		{"short frame", eth(recMAC, 0x0800, nil)[:20], [4]bool{}},
		{"runt", recMAC, [4]bool{}},
	}
	progs := make([][]Insn, len(modes))
	for i, m := range modes {
		p, err := Program(m.p)
		if err != nil {
			t.Fatalf("%s: %v", m.name, err)
		}
		progs[i] = p
	}
	for _, c := range cases {
		for i, m := range modes {
			got := run(t, progs[i], c.frame)
			if (got != 0) != c.want[i] {
				t.Errorf("%s / %s: returned %d, want pass=%v", c.name, m.name, got, c.want[i])
			}
			if got != 0 && got != Accept {
				t.Errorf("%s / %s: a match returns %d, which would truncate the frame", c.name, m.name, got)
			}
		}
	}
}

func TestProgramRejects(t *testing.T) {
	bad := []Params{
		{MAC: net.HardwareAddr{0x03, 0, 0, 0, 0, 1}},
		{MAC: recMAC[:5]},
		{MAC: recMAC, IPv4: zeroIP},
		{MAC: recMAC, IPv4: netip.MustParseAddr("2001:db8::1")},
		{MAC: recMAC, IPv4: netip.MustParseAddr("224.0.0.1")},
		{MAC: recMAC, IPv4: netip.MustParseAddr("255.255.255.255")},
	}
	for _, p := range bad {
		if _, err := Program(p); err == nil {
			t.Errorf("Program(%+v) accepted", p)
		}
	}
}

// TestProgramFitsTheKernel: below net.bpf.maxinsns, every jump forward
// and inside the program, and a return last, as bpf_validate() requires.
func TestProgramFitsTheKernel(t *testing.T) {
	for _, m := range modes {
		p, err := Program(m.p)
		if err != nil {
			t.Fatal(err)
		}
		if len(p) >= MaxInsns || p[len(p)-1].Code != opRetK {
			t.Errorf("%s: %d insns, last op 0x%x", m.name, len(p), p[len(p)-1].Code)
		}
		for i, in := range p {
			switch in.Code {
			case opJeqK, opJgeK, opJsetK:
				if i+1+int(in.Jt) >= len(p) || i+1+int(in.Jf) >= len(p) {
					t.Errorf("%s: jump at %d leaves the program", m.name, i)
				}
			}
		}
		t.Logf("%s: %d instructions", m.name, len(p))
	}
}

func TestProgramIsDeterministic(t *testing.T) {
	a, _ := Program(modes[0].p)
	b, _ := Program(Params{MAC: net.HardwareAddr{0x02, 0x5e, 0x00, 0x0a, 0x00, 0x10}, IPv4: netip.MustParseAddr("192.0.2.10")})
	if !Equal(a, b) {
		t.Fatal("the same NIC gave two programs; the health check compares them")
	}
	c, _ := Program(modes[1].p)
	if Equal(a, c) {
		t.Fatal("static and DHCP programs are equal")
	}
}
