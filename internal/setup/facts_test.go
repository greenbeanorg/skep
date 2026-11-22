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
