//go:build skep_testtasks

// Command ngbpfgen generates the ng_bpf(4) anti-spoofing filter for one guest
// port, for the M7c netgraph probe (tests/integration/jails/results/
// m7c-probe.md, part 2). It prints an ngctl(8) script that hangs an ng_bpf
// node between the host end's ng_ether(4) "lower" and "upper" hooks:
//
//	<if>:lower -> [hook "jail"]  ng_bpf  [hook "bridge"] -> <if>:upper
//
// Frames the guest sends arrive on "jail" and are checked by the generated
// program; only matches go on to "bridge" (and from there to if_bridge).
// Frames the host stack itself sends out of <if> (ng_ether diverts them to
// "upper") arrive on "bridge" and pass to "jail" unchecked.
//
// The program is default deny. Static mode: the Ethernet source must be the
// recorded MAC; IPv4 must come from the configured address; ARP must be
// Ethernet/IPv4 with sender MAC = recorded MAC and sender IP = configured
// address or 0.0.0.0 (RFC 5227 probe); everything else (IPv6, an 802.1Q tag
// left in the frame data, any other ethertype) is dropped. DHCP mode: the
// same, except IPv4 source and ARP sender IP are not checked.
//
// It is test tooling for the probe and the ADR, built only with the
// skep_testtasks tag and never part of a shipped binary.
//
// Usage:
//
//	ngbpfgen -if epair0a -mac 02:5e:00:0a:00:10 -ip 192.0.2.10 [-mode static|dhcp] [-node NAME] [-format ngctl|asm|setprogram]
package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// Classic BPF opcodes used here (sys/net/bpf.h).
const (
	ldW  = 0x20 // BPF_LD|BPF_W|BPF_ABS
	ldH  = 0x28 // BPF_LD|BPF_H|BPF_ABS
	ldB  = 0x30 // BPF_LD|BPF_B|BPF_ABS
	jeqK = 0x15 // BPF_JMP|BPF_JEQ|BPF_K
	retK = 0x06 // BPF_RET|BPF_K
)

// acceptLen is returned for a match. ng_bpf truncates a matching frame to
// the returned length, so this must be at least any frame length.
const acceptLen = 0xffffffff

// Mode selects what the filter pins.
type Mode string

// Modes.
const (
	// Static pins the MAC, the IPv4 source and the ARP sender MAC and IP.
	Static Mode = "static"
	// DHCP pins the MAC and the ARP sender MAC only.
	DHCP Mode = "dhcp"
)

// Insn is one classic BPF instruction (struct bpf_insn).
type Insn struct {
	Code   uint16
	Jt, Jf uint8
	K      uint32
}

// asm is a tiny assembler with symbolic jump targets, so the program below
// reads top to bottom and the offsets are computed, not hand-counted.
type asm struct {
	insns  []Insn
	labels map[string]int
	fixups []fixup
}

type fixup struct {
	at     int
	jt, jf string
}

func (a *asm) emit(code uint16, k uint32) { a.insns = append(a.insns, Insn{Code: code, K: k}) }

// jeq compares A with k and jumps to label t if equal, f otherwise. An
// empty label means the next instruction.
func (a *asm) jeq(k uint32, t, f string) {
	a.fixups = append(a.fixups, fixup{at: len(a.insns), jt: t, jf: f})
	a.insns = append(a.insns, Insn{Code: jeqK, K: k})
}

func (a *asm) label(name string) { a.labels[name] = len(a.insns) }

func (a *asm) link() ([]Insn, error) {
	off := func(at int, l string) (uint8, error) {
		if l == "" {
			return 0, nil
		}
		to, ok := a.labels[l]
		if !ok {
			return 0, fmt.Errorf("ngbpfgen: undefined label %q", l)
		}
		d := to - at - 1
		if d < 0 || d > 255 {
			return 0, fmt.Errorf("ngbpfgen: jump to %q out of range (%d)", l, d)
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
	return a.insns, nil
}

// Program returns the filter for frames coming from the guest.
//
// Offsets are from the start of the Ethernet frame as ng_ether hands it
// over. A single 802.1Q tag has already been moved out of the frame data
// into the mbuf (M_VLANTAG) by ether_input_internal(), so the program cannot
// see it; see the probe notes. Loads past the end of the frame make
// bpf_filter() return 0, so short frames are dropped.
func Program(mac net.HardwareAddr, ip netip.Addr, mode Mode) ([]Insn, error) {
	if len(mac) != 6 {
		return nil, fmt.Errorf("ngbpfgen: MAC must be 6 bytes, got %v", mac)
	}
	if mac[0]&1 != 0 {
		return nil, fmt.Errorf("ngbpfgen: %v is a multicast MAC", mac)
	}
	if mode != Static && mode != DHCP {
		return nil, fmt.Errorf("ngbpfgen: mode must be %q or %q", Static, DHCP)
	}
	if mode == Static && (!ip.Is4() || ip.IsUnspecified()) {
		return nil, fmt.Errorf("ngbpfgen: static mode needs a unicast IPv4 address, got %v", ip)
	}
	macHi := binary.BigEndian.Uint32(mac[0:4])
	macLo := uint32(binary.BigEndian.Uint16(mac[4:6]))
	var ip32 uint32
	if mode == Static {
		b := ip.As4()
		ip32 = binary.BigEndian.Uint32(b[:])
	}

	a := &asm{labels: map[string]int{}}
	// Ethernet source MAC (bytes 6-11).
	a.emit(ldW, 6)
	a.jeq(macHi, "", "drop")
	a.emit(ldH, 10)
	a.jeq(macLo, "", "drop")
	// Ethertype (bytes 12-13).
	a.emit(ldH, 12)
	a.jeq(0x0800, "ipv4", "")
	a.jeq(0x0806, "arp", "drop")

	a.label("ipv4")
	// Load the last word of a minimal IPv4 header (destination, 14 + 16).
	// The value is not used: the load only makes bpf_filter() return 0 for
	// a frame too short to hold a whole header.
	a.emit(ldW, 30)
	if mode == Static {
		a.emit(ldW, 26) // IPv4 source: 14 + 12
		a.jeq(ip32, "accept", "drop")
	} else {
		a.emit(retK, acceptLen)
	}

	a.label("arp")
	a.emit(ldW, 14) // htype 1 (Ethernet), ptype 0x0800
	a.jeq(0x00010800, "", "drop")
	a.emit(ldH, 18) // hlen 6, plen 4
	a.jeq(0x0604, "", "drop")
	a.emit(ldW, 22) // sender MAC: 14 + 8
	a.jeq(macHi, "", "drop")
	a.emit(ldH, 26)
	if mode == Static {
		a.jeq(macLo, "", "drop")
		a.emit(ldW, 28) // sender IP: 14 + 14
		a.jeq(ip32, "accept", "")
		a.jeq(0, "accept", "drop") // RFC 5227 probe
	} else {
		a.jeq(macLo, "accept", "drop")
	}

	a.label("accept")
	a.emit(retK, acceptLen)
	a.label("drop")
	a.emit(retK, 0)
	return a.link()
}

// passAll matches every frame.
var passAll = []Insn{{Code: retK, K: acceptLen}}

// Hook names on the ng_bpf node.
const (
	hookJail   = "jail"
	hookBridge = "bridge"
)

// setprogram renders an ngctl "setprogram" argument (struct ng_bpf_hookprog
// in ng_parse ASCII form).
func setprogram(this, match, nomatch string, p []Insn) string {
	var b strings.Builder
	fmt.Fprintf(&b, "{ thisHook=%q ifMatch=%q ifNotMatch=%q bpf_prog_len=%d bpf_prog=[", this, match, nomatch, len(p))
	for _, in := range p {
		fmt.Fprintf(&b, " { code=%d jt=%d jf=%d k=%d }", in.Code, in.Jt, in.Jf, in.K)
	}
	b.WriteString(" ] }")
	return b.String()
}

// Script returns an ngctl -f script that installs the filter on ifName.
//
// Order matters for failing closed: the "jail" hook is connected to lower
// first. A new ng_bpf hook drops everything until its program is set, so
// from that moment until the real program is loaded, frames from the guest
// are dropped, never passed. The "bridge" hook is connected next (host
// output on ifName is dropped until its pass-all program is set). Programs
// are loaded last.
func Script(ifName, node string, prog []Insn) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mkpeer %s: bpf lower %s\n", ifName, hookJail)
	fmt.Fprintf(&b, "name %s:lower %s\n", ifName, node)
	fmt.Fprintf(&b, "connect %s: %s: upper %s\n", ifName, node, hookBridge)
	fmt.Fprintf(&b, "msg %s: setprogram %s\n", node, setprogram(hookJail, hookBridge, "", prog))
	fmt.Fprintf(&b, "msg %s: setprogram %s\n", node, setprogram(hookBridge, hookJail, "", passAll))
	return b.String()
}

// Listing returns a human-readable listing of p.
func Listing(p []Insn) string {
	var b strings.Builder
	for i, in := range p {
		var s string
		switch in.Code {
		case ldW:
			s = fmt.Sprintf("ld   [%d]", in.K)
		case ldH:
			s = fmt.Sprintf("ldh  [%d]", in.K)
		case ldB:
			s = fmt.Sprintf("ldb  [%d]", in.K)
		case jeqK:
			s = fmt.Sprintf("jeq  #0x%x  jt %d  jf %d", in.K, i+1+int(in.Jt), i+1+int(in.Jf))
		case retK:
			s = fmt.Sprintf("ret  #%d", in.K)
		default:
			s = fmt.Sprintf("op 0x%x k %d", in.Code, in.K)
		}
		fmt.Fprintf(&b, "(%03d) %s\n", i, s)
	}
	return b.String()
}
