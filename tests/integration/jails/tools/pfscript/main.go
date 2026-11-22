//go:build skep_testtasks

// Command pfscript prints, for the M7c validation on hearst, what the agent
// would install on a guest port (internal/portfilter): the ngctl -f script
// for an ether node ID, or the program as a getprogram reply, so a
// hand-made port can carry exactly the agent's filter and a real port's
// program can be compared with it. Test tooling only, built with the
// skep_testtasks tag, never shipped.
//
//	pfscript -id 1a -mac 02:5e:00:0a:00:30 [-ip 192.0.2.30] [-ipv6] [-print script|program]
package main

import (
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/greenbeanorg/skep/internal/portfilter"
)

func main() {
	id := flag.String("id", "", "ether node ID (hex), for -print script")
	mac := flag.String("mac", "", "recorded MAC")
	ip := flag.String("ip", "", "static IPv4 (empty: MAC-only)")
	ipv6 := flag.Bool("ipv6", false, "allow IPv6 MAC-only with the RA guard (every VM port)")
	what := flag.String("print", "script", "script or program")
	flag.Parse()
	hw, err := net.ParseMAC(*mac)
	if err != nil {
		fail(err)
	}
	p := portfilter.Params{MAC: hw, IPv6: *ipv6}
	if *ip != "" {
		if p.IPv4, err = netip.ParseAddr(*ip); err != nil {
			fail(err)
		}
	}
	prog, err := portfilter.Program(p)
	if err != nil {
		fail(err)
	}
	switch *what {
	case "script":
		s, err := portfilter.InstallScript(portfilter.NodeID(*id), prog)
		if err != nil {
			fail(err)
		}
		fmt.Print(s)
	case "program":
		fmt.Println(portfilter.SetProgram(portfilter.HookJail, portfilter.HookBridge, "", prog))
	default:
		fail(fmt.Errorf("-print must be script or program"))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "pfscript:", err)
	os.Exit(1)
}
