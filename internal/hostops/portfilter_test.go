package hostops

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/portfilter"
	"github.com/greenbeanorg/skep/pkg/models"
)

// ngctl show output on 15.1 (M7c probe), for an epair with ID 5 and a
// filter node with ID 19.
const (
	showBareOut     = "  Name: epair3a         Type: ether           ID: 00000005   Num hooks: 0\n"
	showFilteredOut = `  Name: epair3a         Type: ether           ID: 00000005   Num hooks: 2
  Local hook      Peer name       Peer type    Peer ID         Peer hook
  ----------      ---------       ---------    -------         ---------
  upper           skf_epair3a     bpf          00000019        bridge
  lower           skf_epair3a     bpf          00000019        jail
`
)

// filtered answers the ngctl calls of a successful install on ifName for
// params: a bare node by name, the complete filter by ID.
func filtered(run *script, ifName string, params portfilter.Params) {
	prog, _ := portfilter.Program(params)
	run.answers["ngctl show "+ifName+":"] = strings.ReplaceAll(showBareOut, "epair3a", ifName)
	run.answers["ngctl show [5]:"] = strings.ReplaceAll(showFilteredOut, "epair3a", ifName)
	run.answers[`ngctl msg [19]: getprogram "jail"`] = "Rec'd response \"getprogram\" (2) from \"[19]:\":\nArgs:\t" +
		portfilter.SetProgram("jail", "bridge", "", prog) + "\n"
	run.answers[`ngctl msg [19]: getprogram "bridge"`] = "Args:\t" + portfilter.SetProgram("bridge", "jail", "", portfilter.PassAll) + "\n"
}

func ngErr(msg string) error {
	return &CommandError{Name: "ngctl", ExitCode: 1, Stderr: "ngctl: " + msg, Err: errors.New("exit status 1")}
}

var vmParams = portfilter.Params{MAC: []byte{2, 0x5b, 0, 0, 0, 9}, IPv6: true}

func TestPortFilterAvailable(t *testing.T) {
	h, run, _ := jailHost(t)
	if err := h.PortFilterAvailable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(run.calls, []string{"kldstat -q -m netgraph", "kldstat -q -m ng_socket", "kldstat -q -m ng_ether", "kldstat -q -m ng_bpf"}) {
		t.Errorf("calls %q", run.calls)
	}
	h, run, _ = jailHost(t)
	run.answers["kldstat -q -m ng_bpf"] = &CommandError{Name: "kldstat", ExitCode: 1, Err: errors.New("exit status 1")}
	err := h.PortFilterAvailable(context.Background())
	wantCode(t, err, models.CodePortFilterUnavailable)
	if !strings.Contains(err.Error(), "ng_bpf") || !strings.Contains(err.Error(), "kld_list") {
		t.Errorf("the refusal does not say what to load: %v", err)
	}
	for _, c := range run.calls {
		if strings.HasPrefix(c, "kldload") || strings.HasPrefix(c, "ngctl") {
			t.Fatalf("loaded or touched netgraph: %q", c)
		}
	}
}

func TestCheckJailNetRefusesWithoutNetgraph(t *testing.T) {
	h, run, _ := jailHost(t)
	run.answers["kldstat -q -m ng_ether"] = &CommandError{Name: "kldstat", ExitCode: 1, Err: errors.New("exit status 1")}
	spec := jailSpec()
	spec.NICs = []JailNIC{{Slot: 0, Bridge: "bridge0", MAC: "02:00:00:00:00:01", DHCP: true}}
	wantCode(t, h.checkJailNet(context.Background(), spec), models.CodePortFilterUnavailable)
}

func TestInspectPortFilter(t *testing.T) {
	h, run, _ := jailHost(t)
	filtered(run, "tap3", vmParams)
	run.answers["ngctl show tap3:"] = strings.ReplaceAll(showFilteredOut, "epair3a", "tap3")
	st, err := h.InspectPortFilter(context.Background(), "tap3", vmParams)
	if err != nil || st.Gone || st.Ether != "5" || st.Problem != "" {
		t.Fatalf("complete filter: %+v, %v", st, err)
	}
	// Another program for the same port (a pass-all, or another NIC's).
	st, _ = h.InspectPortFilter(context.Background(), "tap3", portfilter.Params{MAC: vmParams.MAC})
	if st.Problem == "" {
		t.Error("a different program was accepted")
	}
	// No filter at all.
	run.answers["ngctl show tap3:"] = strings.ReplaceAll(showBareOut, "epair3a", "tap3")
	if st, _ = h.InspectPortFilter(context.Background(), "tap3", vmParams); !strings.Contains(st.Problem, "no filter") {
		t.Errorf("bare port: %+v", st)
	}
	// The interface is gone.
	run.answers["ngctl show tap3:"] = ngErr("send msg: No such file or directory")
	if st, err = h.InspectPortFilter(context.Background(), "tap3", vmParams); err != nil || !st.Gone {
		t.Errorf("gone port: %+v, %v", st, err)
	}
	if _, err := h.InspectPortFilter(context.Background(), "vtnet0", vmParams); err == nil {
		t.Error("a host NIC was inspected")
	}
}

func TestReinstallPortFilter(t *testing.T) {
	// The lower hook was removed: the leftover upper link to the old
	// filter is removed by ID, then the filter is installed by ID.
	h, run, _ := jailHost(t)
	filtered(run, "tap3", vmParams)
	upperOnly := `  Name: tap3            Type: ether           ID: 00000005   Num hooks: 1
  Local hook      Peer name       Peer type    Peer ID         Peer hook
  ----------      ---------       ---------    -------         ---------
  upper           skf_tap3        bpf          00000019        bridge
`
	run.answers["ngctl show [5]:"] = []any{upperOnly, strings.ReplaceAll(showFilteredOut, "epair3a", "tap3")}
	if err := h.ReinstallPortFilter(context.Background(), "tap3", "5", vmParams); err != nil {
		t.Fatal(err)
	}
	want := []string{"ngctl show [5]:", "ngctl rmhook [5]: upper", "ngctl -f -", "ngctl name [5]:lower skf_tap3",
		"ngctl show [5]:", `ngctl msg [19]: getprogram "jail"`, `ngctl msg [19]: getprogram "bridge"`}
	if !slices.Equal(run.calls, want) {
		t.Fatalf("calls:\n%q\nwant\n%q", run.calls, want)
	}

	// A foreign node on lower is left alone; the install then fails.
	h, run, _ = jailHost(t)
	filtered(run, "tap3", vmParams)
	foreign := `  Name: tap3            Type: ether           ID: 00000005   Num hooks: 1
  Local hook      Peer name       Peer type    Peer ID         Peer hook
  ----------      ---------       ---------    -------         ---------
  lower           <unnamed>       socket       00000030        hook
`
	run.answers["ngctl show [5]:"] = foreign
	run.answers["ngctl -f -"] = ngErr("line 1: error in file")
	wantCode(t, h.ReinstallPortFilter(context.Background(), "tap3", "5", vmParams), models.CodePortFilterFailed)
	for _, c := range run.calls {
		if strings.Contains(c, "rmhook") {
			t.Fatalf("removed a foreign hook: %q", c)
		}
	}

	// The node ID now belongs to another interface (the name was reused
	// or the ID recycled): nothing is touched.
	h, run, _ = jailHost(t)
	run.answers["ngctl show [5]:"] = strings.ReplaceAll(showBareOut, "epair3a", "tap4")
	wantCode(t, h.ReinstallPortFilter(context.Background(), "tap3", "5", vmParams), models.CodePortFilterFailed)
	if len(run.calls) != 1 {
		t.Errorf("went on with another interface's node: %q", run.calls)
	}
}

func TestDetachPort(t *testing.T) {
	h, run, _ := jailHost(t)
	if err := h.DetachPort(context.Background(), "bridge1", "tap3"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(run.calls, []string{"ifconfig bridge1 deletem tap3"}) {
		t.Errorf("calls %q", run.calls)
	}
	if h.DetachPort(context.Background(), "bridge1", "vtnet1") == nil || h.DetachPort(context.Background(), "bridge1; x", "tap3") == nil {
		t.Error("an invalid detach was accepted")
	}
}

func TestParseBridgeVLAN(t *testing.T) {
	// ifconfig bridge1 on hearst with vlanfilter and one PVID (M7c probe).
	out := `bridge1: flags=1008843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST,LOWER_UP> metric 0 mtu 1500
	options=10<VLAN_HWTAGGING>
	ether 58:9c:fc:10:8c:43
	bridge flags=1<VLANFILTER>
	member: epair1a flags=143<LEARNING,DISCOVER,AUTOEDGE,AUTOPTP>
	        port 9 priority 128 path cost 2000 vlan protocol 802.1q
	member: epair0a flags=143<LEARNING,DISCOVER,AUTOEDGE,AUTOPTP>
	        port 6 priority 128 path cost 2000 vlan protocol 802.1q untagged 1
	groups: bridge
`
	b := parseBridgeVLAN([]byte(out))
	if !b.Filtering || b.PVID["epair0a"] != 1 || len(b.PVID) != 1 {
		t.Fatalf("%+v", b)
	}
	if b = parseBridgeVLAN([]byte("bridge0: flags=8843<UP> mtu 1500\n\tmember: vtnet1 flags=143<LEARNING>\n\t        port 2 priority 128 path cost 2000\n")); b.Filtering || len(b.PVID) != 0 {
		t.Errorf("unfiltered bridge: %+v", b)
	}
}

func TestLockPort(t *testing.T) {
	h, _, _ := jailHost(t)
	h.conf.RunDir = t.TempDir()
	unlock, err := h.LockPort(PortJail, ctrA, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	// Another open of the same lock (as another process would) is busy.
	if _, err := h.LockPort(PortJail, ctrA, 0, false); !errors.Is(err, ErrPortBusy) {
		t.Fatalf("second lock: %v, want ErrPortBusy", err)
	}
	// Other slots, other owners, and the other kind are separate locks.
	for _, k := range []struct {
		kind PortOwner
		id   string
		slot int
	}{{PortJail, ctrA, 1}, {PortJail, ctrB, 0}, {PortVM, ctrA, 0}} {
		u, err := h.LockPort(k.kind, k.id, k.slot, false)
		if err != nil {
			t.Fatalf("%+v: %v", k, err)
		}
		u()
	}
	// A waiting lock gets it once released.
	got := make(chan error, 1)
	go func() {
		u, err := h.LockPort(PortJail, ctrA, 0, true)
		if err == nil {
			u()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("a waiting lock returned while held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	if _, err := h.LockPort(PortJail, "../x", 0, false); err == nil {
		t.Error("an invalid owner ID was accepted")
	}
}
