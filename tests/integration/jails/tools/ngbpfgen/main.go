//go:build skep_testtasks

package main

import (
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
)

// nameRE limits interface and node names to what netgraph accepts in a
// path and what Skep generates; no ':', '.', '[' or spaces.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,15}$`)

func main() {
	ifName := flag.String("if", "", "host end of the guest port (e.g. epair0a or tap0)")
	macS := flag.String("mac", "", "recorded guest MAC")
	ipS := flag.String("ip", "", "configured guest IPv4 (static mode)")
	mode := flag.String("mode", string(Static), "static or dhcp")
	node := flag.String("node", "", "ng_bpf node name (default skf_<if>)")
	format := flag.String("format", "ngctl", "ngctl (script for ngctl -f), asm (listing) or setprogram (jail hook only)")
	flag.Parse()

	if !nameRE.MatchString(*ifName) {
		fail(fmt.Errorf("-if: bad interface name %q", *ifName))
	}
	if *node == "" {
		*node = "skf_" + *ifName
	}
	if !nameRE.MatchString(*node) {
		fail(fmt.Errorf("-node: bad node name %q", *node))
	}
	mac, err := net.ParseMAC(*macS)
	if err != nil {
		fail(fmt.Errorf("-mac: %w", err))
	}
	var ip netip.Addr
	if *ipS != "" {
		if ip, err = netip.ParseAddr(*ipS); err != nil {
			fail(fmt.Errorf("-ip: %w", err))
		}
	}
	prog, err := Program(mac, ip, Mode(*mode))
	if err != nil {
		fail(err)
	}
	switch *format {
	case "ngctl":
		fmt.Print(Script(*ifName, *node, prog))
	case "asm":
		fmt.Print(Listing(prog))
	case "setprogram":
		fmt.Println(setprogram(hookJail, hookBridge, "", prog))
	default:
		fail(fmt.Errorf("-format must be ngctl, asm or setprogram"))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "ngbpfgen:", err)
	os.Exit(1)
}
