//go:build skep_testtasks && freebsd

package main

import (
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

func main() {
	ifName := flag.String("if", "", "interface to write to (inside the jail: skep0)")
	mode := flag.String("mode", "", "arp, icmp, dhcp or ipv6")
	ethSrc := flag.String("eth-src", "", "Ethernet source MAC (may be forged)")
	ethDst := flag.String("eth-dst", "ff:ff:ff:ff:ff:ff", "Ethernet destination MAC")
	op := flag.String("op", "request", "ARP operation: request or reply")
	sha := flag.String("sha", "", "ARP sender MAC (payload)")
	spa := flag.String("spa", "", "ARP sender IPv4 (payload)")
	tha := flag.String("tha", "00:00:00:00:00:00", "ARP target MAC (payload)")
	tpa := flag.String("tpa", "", "ARP target IPv4 (payload)")
	src := flag.String("src", "", "IPv4 source (icmp)")
	dst := flag.String("dst", "", "IPv4 destination (icmp)")
	count := flag.Int("count", 1, "frames to send, one per second")
	vlan := flag.String("vlan", "", "802.1Q VLAN IDs to insert, outermost first (e.g. 5 or 5,7)")
	flag.Parse()

	frame, err := build(*mode, *ethSrc, *ethDst, *op, *sha, *spa, *tha, *tpa, *src, *dst)
	if err != nil {
		fail(err)
	}
	if *vlan != "" {
		var vids []uint16
		for _, f := range strings.Split(*vlan, ",") {
			v, err := strconv.ParseUint(f, 10, 16)
			if err != nil {
				fail(fmt.Errorf("-vlan: %w", err))
			}
			vids = append(vids, uint16(v))
		}
		if frame, err = tagVLAN(frame, vids); err != nil {
			fail(err)
		}
	}
	fd, err := openBPF(*ifName)
	if err != nil {
		fail(err)
	}
	defer syscall.Close(fd)
	for i := 0; i < *count; i++ {
		if i > 0 {
			time.Sleep(time.Second)
		}
		n, err := syscall.Write(fd, frame)
		if err != nil {
			fail(fmt.Errorf("write %d: %w", i+1, err))
		}
		fmt.Printf("rawl2: %s frame %d/%d, %d bytes, written to %s\n", *mode, i+1, *count, n, *ifName)
	}
}

func build(mode, ethSrc, ethDst, op, sha, spa, tha, tpa, src, dst string) ([]byte, error) {
	es, err := net.ParseMAC(ethSrc)
	if err != nil {
		return nil, fmt.Errorf("-eth-src: %w", err)
	}
	ed, err := net.ParseMAC(ethDst)
	if err != nil {
		return nil, fmt.Errorf("-eth-dst: %w", err)
	}
	switch mode {
	case "arp":
		var o uint16 = 1
		if op == "reply" {
			o = 2
		}
		sh, err := net.ParseMAC(sha)
		if err != nil {
			return nil, fmt.Errorf("-sha: %w", err)
		}
		th, err := net.ParseMAC(tha)
		if err != nil {
			return nil, fmt.Errorf("-tha: %w", err)
		}
		sp, err := netip.ParseAddr(spa)
		if err != nil {
			return nil, fmt.Errorf("-spa: %w", err)
		}
		tp, err := netip.ParseAddr(tpa)
		if err != nil {
			return nil, fmt.Errorf("-tpa: %w", err)
		}
		p, err := arpPayload(o, sh, sp, th, tp)
		if err != nil {
			return nil, err
		}
		return ethernet(ed, es, etherARP, p)
	case "icmp":
		s, err := netip.ParseAddr(src)
		if err != nil {
			return nil, fmt.Errorf("-src: %w", err)
		}
		d, err := netip.ParseAddr(dst)
		if err != nil {
			return nil, fmt.Errorf("-dst: %w", err)
		}
		p, err := ipv4(s, d, 1, 0x5303, icmpEcho(0x5303, 1))
		if err != nil {
			return nil, err
		}
		return ethernet(ed, es, etherIPv4, p)
	case "dhcp":
		p, err := ipv4(netip.IPv4Unspecified(), netip.MustParseAddr("255.255.255.255"), 17, 0x5304,
			dhcpDiscover(es, 0x5304c0de))
		if err != nil {
			return nil, err
		}
		return ethernet(broadcast, es, etherIPv4, p)
	case "ipv6":
		// Link-local source from the MAC (modified EUI-64), to all nodes.
		eui := [16]byte{0: 0xfe, 1: 0x80, 8: es[0] ^ 2, 9: es[1], 10: es[2], 11: 0xff, 12: 0xfe, 13: es[3], 14: es[4], 15: es[5]}
		p, err := ipv6Echo(netip.AddrFrom16(eui), netip.MustParseAddr("ff02::1"), 0x5305, 1)
		if err != nil {
			return nil, err
		}
		return ethernet(net.HardwareAddr{0x33, 0x33, 0, 0, 0, 1}, es, etherIPv6, p)
	}
	return nil, fmt.Errorf("-mode must be arp, icmp, dhcp or ipv6")
}

// openBPF opens /dev/bpf bound to ifName with BIOCSHDRCMPLT set, so the
// kernel keeps the frame's own source MAC instead of filling in the
// interface's.
func openBPF(ifName string) (int, error) {
	fd, err := syscall.Open("/dev/bpf", syscall.O_RDWR, 0)
	if err != nil {
		return -1, fmt.Errorf("open /dev/bpf: %w", err)
	}
	var ifr [32]byte // struct ifreq: name[16] + union
	if len(ifName) == 0 || len(ifName) >= 16 {
		syscall.Close(fd)
		return -1, fmt.Errorf("-if: bad interface name %q", ifName)
	}
	copy(ifr[:], ifName)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.BIOCSETIF, uintptr(unsafe.Pointer(&ifr[0]))); e != 0 {
		syscall.Close(fd)
		return -1, fmt.Errorf("BIOCSETIF %s: %w", ifName, e)
	}
	one := uint32(1)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.BIOCSHDRCMPLT, uintptr(unsafe.Pointer(&one))); e != 0 {
		syscall.Close(fd)
		return -1, fmt.Errorf("BIOCSHDRCMPLT: %w", e)
	}
	return fd, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "rawl2:", err)
	os.Exit(1)
}
