package setup

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestParseIfconfigInet(t *testing.T) {
	out := `vtnet0: flags=1008843<UP,BROADCAST,RUNNING> metric 0 mtu 1500
	options=ec079b<RXCSUM>
	inet 10.79.20.170/24 broadcast 10.79.20.255
	inet 10.79.20.171/32 broadcast 10.79.20.171
	inet6 fe80::1%vtnet0/64 scopeid 0x1
	inet bogus
`
	got := parseIfconfigInet(out)
	if len(got) != 2 || got[0].String() != "10.79.20.170/24" || got[1].String() != "10.79.20.171/32" {
		t.Fatalf("got %v", got)
	}
	if parseIfconfigInet("") != nil {
		t.Fatal("addresses from nothing")
	}
}

func TestParseZpoolList(t *testing.T) {
	got := parseZpoolList("zroot\tONLINE\nskeptest\tDEGRADED\n\nodd line here\n")
	if !reflect.DeepEqual(got, map[string]string{"zroot": "ONLINE", "skeptest": "DEGRADED"}) {
		t.Fatalf("got %v", got)
	}
}

func TestLines(t *testing.T) {
	if got := lines("skep-b\n\n  skep-a \nwww\n"); !reflect.DeepEqual(got, []string{"skep-a", "skep-b", "www"}) {
		t.Fatalf("got %v", got)
	}
}

func TestOutputRunsWithoutShell(t *testing.T) {
	// The argument would be a command substitution in a shell.
	out, err := output(context.Background(), "echo", "$(id)")
	if err != nil || out != "$(id)\n" {
		t.Fatalf("%q %v", out, err)
	}
	if commandTimeout < time.Second {
		t.Fatal("timeout too short")
	}
}

// TestParseIfconfigBridge: the output of "ifconfig bridge9" on smith
// (FreeBSD 15.1-RELEASE-p7, 2026-11-22), with vlanfilter off and on.
func TestParseIfconfigBridge(t *testing.T) {
	const head = `bridge9: flags=1008843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST,LOWER_UP> metric 0 mtu 1500
	options=10<VLAN_HWTAGGING>
	ether 58:9c:fc:10:a1:c0
	id 00:00:00:00:00:00 priority 32768 hellotime 2 fwddelay 15
	maxage 20 holdcnt 6 proto rstp maxaddr 2000 timeout 1200
	root id 00:00:00:00:00:00 priority 32768 ifcost 0 port 0
`
	const tail = `	member: vtnet1 flags=143<LEARNING,DISCOVER,AUTOEDGE,AUTOPTP>
	        port 2 priority 128 path cost 2000 vlan protocol 802.1q
	member: epair0a flags=143<LEARNING,DISCOVER,AUTOEDGE,AUTOPTP>
	        port 3 priority 128 path cost 2000 vlan protocol 802.1q
	groups: bridge
	nd6 options=9<PERFORMNUD,IFDISABLED>
`
	off := parseIfconfigBridge(head + "\tbridge flags=0<>\n" + tail)
	if !reflect.DeepEqual(off, Bridge{Members: []string{"vtnet1", "epair0a"}}) {
		t.Fatalf("off: %+v", off)
	}
	on := parseIfconfigBridge(head + "\tbridge flags=1<VLANFILTER>\n" + tail)
	if !on.VLANFilter || len(on.Members) != 2 {
		t.Fatalf("on: %+v", on)
	}
	if b := parseIfconfigBridge(head + "\tbridge flags=0<>\n\tgroups: bridge\n"); b.Members != nil || b.VLANFilter {
		t.Fatalf("empty: %+v", b)
	}
}

func TestBridgeOf(t *testing.T) {
	f := Facts{Bridges: map[string]Bridge{"bridge9": {Members: []string{"vtnet1"}}, "bridge1": {Members: []string{"epair0a"}}}}
	if n, ok := f.BridgeOf("vtnet1"); !ok || n != "bridge9" {
		t.Fatalf("vtnet1: %q %v", n, ok)
	}
	if _, ok := f.BridgeOf("vtnet2"); ok {
		t.Fatal("vtnet2 is in no bridge")
	}
}
