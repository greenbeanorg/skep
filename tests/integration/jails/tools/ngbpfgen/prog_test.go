//go:build skep_testtasks

package main

import (
	"encoding/binary"
	"net"
	"net/netip"
	"strings"
	"testing"
)

// run interprets p the way sys/net/bpf_filter.c does for the opcodes
// Program uses: a load past the end of the packet returns 0.
func run(t *testing.T, p []Insn, pkt []byte) uint32 {
	t.Helper()
	var a uint32
	for pc := 0; pc < len(p); pc++ {
		in := p[pc]
		k := int(in.K)
		switch in.Code {
		case ldW:
			if k+4 > len(pkt) {
				return 0
			}
			a = binary.BigEndian.Uint32(pkt[k:])
		case ldH:
			if k+2 > len(pkt) {
				return 0
			}
			a = uint32(binary.BigEndian.Uint16(pkt[k:]))
		case ldB:
			if k+1 > len(pkt) {
				return 0
			}
			a = uint32(pkt[k])
		case jeqK:
			if a == in.K {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case retK:
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
	// nearMAC differs from recMAC only in the last two bytes, to catch a
	// program that checks only the first four.
	nearMAC  = net.HardwareAddr{0x02, 0x5e, 0x00, 0x0a, 0x00, 0x11}
	bcast    = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	recIP    = netip.MustParseAddr("192.0.2.10")
	otherIP  = netip.MustParseAddr("192.0.2.50")
	victimIP = netip.MustParseAddr("192.0.2.20")
	zeroIP   = netip.IPv4Unspecified()
)

func eth(src net.HardwareAddr, etype uint16, payload []byte) []byte {
	b := make([]byte, 14, 60)
	copy(b[0:6], bcast)
	copy(b[6:12], src)
	binary.BigEndian.PutUint16(b[12:], etype)
	b = append(b, payload...)
	for len(b) < 60 {
		b = append(b, 0)
	}
	return b
}

func ip4(src netip.Addr) []byte {
	b := make([]byte, 28)
	b[0] = 0x45
	b[9] = 1
	s, d := src.As4(), victimIP.As4()
	copy(b[12:], s[:])
	copy(b[16:], d[:])
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

func vlan(f []byte, vid uint16) []byte {
	out := append([]byte{}, f[:12]...)
	out = binary.BigEndian.AppendUint16(out, 0x8100)
	out = binary.BigEndian.AppendUint16(out, vid)
	return append(out, f[12:]...)
}

func TestProgram(t *testing.T) {
	badARPHlen := arp(recMAC, recIP)
	badARPHlen[4] = 8
	cases := []struct {
		name         string
		frame        []byte
		static, dhcp bool // expected pass
	}{
		{"ipv4 ok", eth(recMAC, 0x0800, ip4(recIP)), true, true},
		{"A forged eth src", eth(forgedMAC, 0x0800, ip4(recIP)), false, false},
		{"A near eth src (last 2 bytes)", eth(nearMAC, 0x0800, ip4(recIP)), false, false},
		{"B forged ipv4 src", eth(recMAC, 0x0800, ip4(otherIP)), false, true},
		{"E dhcp from 0.0.0.0", eth(recMAC, 0x0800, ip4(zeroIP)), false, true},
		{"E dhcp from forged MAC", eth(forgedMAC, 0x0800, ip4(zeroIP)), false, false},
		{"arp ok", eth(recMAC, 0x0806, arp(recMAC, recIP)), true, true},
		{"arp probe 0.0.0.0", eth(recMAC, 0x0806, arp(recMAC, zeroIP)), true, true},
		{"C1 arp forged sender ip", eth(recMAC, 0x0806, arp(recMAC, otherIP)), false, true},
		{"C2 arp forged sender mac", eth(recMAC, 0x0806, arp(forgedMAC, recIP)), false, false},
		{"C2 arp near sender mac", eth(recMAC, 0x0806, arp(nearMAC, recIP)), false, false},
		{"C4 arp claims victim ip", eth(recMAC, 0x0806, arp(recMAC, victimIP)), false, true},
		{"arp bad hlen", eth(recMAC, 0x0806, badARPHlen), false, false},
		{"arp forged eth src", eth(forgedMAC, 0x0806, arp(recMAC, recIP)), false, false},
		{"F ipv6", eth(recMAC, 0x86dd, make([]byte, 40)), false, false},
		{"vlan tag in data", vlan(eth(recMAC, 0x0800, ip4(recIP)), 5), false, false},
		{"qinq ethertype", eth(recMAC, 0x88a8, make([]byte, 40)), false, false},
		{"other ethertype", eth(recMAC, 0x88cc, make([]byte, 40)), false, false},
		{"short frame", eth(recMAC, 0x0800, nil)[:20], false, false},
		{"runt", recMAC, false, false},
	}
	ps, err := Program(recMAC, recIP, Static)
	if err != nil {
		t.Fatal(err)
	}
	pd, err := Program(recMAC, netip.Addr{}, DHCP)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		for _, m := range []struct {
			mode string
			p    []Insn
			want bool
		}{{"static", ps, c.static}, {"dhcp", pd, c.dhcp}} {
			got := run(t, m.p, c.frame)
			if (got != 0) != m.want {
				t.Errorf("%s / %s: returned %d, want pass=%v", c.name, m.mode, got, m.want)
			}
			if got != 0 && got != acceptLen {
				t.Errorf("%s / %s: match returns %d, would truncate", c.name, m.mode, got)
			}
		}
	}
}

func TestProgramRejects(t *testing.T) {
	if _, err := Program(recMAC, netip.Addr{}, Static); err == nil {
		t.Error("static without IP accepted")
	}
	if _, err := Program(recMAC, zeroIP, Static); err == nil {
		t.Error("static with 0.0.0.0 accepted")
	}
	if _, err := Program(recMAC, netip.MustParseAddr("2001:db8::1"), Static); err == nil {
		t.Error("static with IPv6 accepted")
	}
	if _, err := Program(net.HardwareAddr{0x03, 0, 0, 0, 0, 1}, recIP, Static); err == nil {
		t.Error("multicast MAC accepted")
	}
	if _, err := Program(recMAC, recIP, "open"); err == nil {
		t.Error("unknown mode accepted")
	}
}

func TestProgramFitsLimits(t *testing.T) {
	// net.bpf.maxinsns is 512 on hearst; bpf_validate() also requires the
	// last instruction to be a return.
	for _, m := range []Mode{Static, DHCP} {
		p, err := Program(recMAC, recIP, m)
		if err != nil {
			t.Fatal(err)
		}
		if len(p) > 512 || p[len(p)-1].Code != retK {
			t.Errorf("%s: %d insns, last op 0x%x", m, len(p), p[len(p)-1].Code)
		}
	}
}

func TestScript(t *testing.T) {
	p, err := Program(recMAC, recIP, Static)
	if err != nil {
		t.Fatal(err)
	}
	s := Script("epair0a", "skf_epair0a", p)
	lines := strings.Split(strings.TrimSpace(s), "\n")
	want := []string{
		"mkpeer epair0a: bpf lower jail",
		"name epair0a:lower skf_epair0a",
		"connect epair0a: skf_epair0a: upper bridge",
		`msg skf_epair0a: setprogram { thisHook="jail" ifMatch="bridge" ifNotMatch=""`,
		`msg skf_epair0a: setprogram { thisHook="bridge" ifMatch="jail" ifNotMatch="" bpf_prog_len=1 bpf_prog=[ { code=6 jt=0 jf=0 k=4294967295 } ] }`,
	}
	if len(lines) != len(want) {
		t.Fatalf("%d lines:\n%s", len(lines), s)
	}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w) {
			t.Errorf("line %d:\n got %s\nwant %s...", i, lines[i], w)
		}
	}
}
