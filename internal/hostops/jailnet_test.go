package hostops

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/pkg/models"
)

const ctrB = "d1d1d1d1-2222-4333-8444-555566667777"

func ifconfigShow(name, desc string) string {
	out := name + ": flags=1008843<UP,BROADCAST,RUNNING> metric 0 mtu 1500\n"
	if desc != "" {
		out += "\tdescription: " + desc + "\n"
	}
	return out + "\tgroups: epair\n"
}

func noSuchInterface(name string) error {
	return &CommandError{Name: "ifconfig", ExitCode: 1, Stderr: "ifconfig: interface " + name + " does not exist", Err: errors.New("exit status 1")}
}

func TestJailInterfaceName(t *testing.T) {
	got, err := JailInterfaceName(ctrA, 0)
	if err != nil || got != "sjc0c0c0c011n0" {
		t.Fatalf("JailInterfaceName = %q, %v", got, err)
	}
	if got, _ := JailInterfaceName(ctrA, 7); got != "sjc0c0c0c011n7" || len(got) > 15 || !jailIfaceRE.MatchString(got) {
		t.Fatalf("slot 7: %q", got)
	}
	for _, slot := range []int{-1, 8} {
		if _, err := JailInterfaceName(ctrA, slot); err == nil {
			t.Fatalf("slot %d accepted", slot)
		}
	}
	if _, err := JailInterfaceName("../x", 0); err == nil {
		t.Fatal("a bad ID was accepted")
	}
	if JailInterfacePrefix(ctrA) != "c0c0c0c011" {
		t.Fatalf("prefix %q", JailInterfacePrefix(ctrA))
	}
}

func TestParseDescription(t *testing.T) {
	for desc, want := range map[string]bool{
		"skep:" + ctrA + ":0": true, "skep:" + ctrA + ":31": true,
		"skep:" + ctrA + ":32": false, "skep:" + ctrA + ":01": false, "skep:" + ctrA: false,
		"skep:x:0": false, "": false, "other:" + ctrA + ":0": false,
	} {
		if _, _, ok := parseDescription(desc); ok != want {
			t.Errorf("parseDescription(%q) = %v, want %v", desc, ok, want)
		}
	}
}

func TestRenderJailConfNICs(t *testing.T) {
	spec := jailSpec()
	spec.NICs = []JailNIC{{Slot: 0, Bridge: "bridge0", MAC: "02:00:00:00:00:01"}, {Slot: 1, Bridge: "bridge0", MAC: "02:00:00:00:00:02"}}
	conf, err := renderJailConf(spec, "/r", "/l")
	if err != nil {
		t.Fatal(err)
	}
	s := string(conf)
	if !strings.Contains(s, "\tvnet.interface = \"sjc0c0c0c011n0\", \"sjc0c0c0c011n1\";\n") || !strings.Contains(s, "\tdevfs_ruleset = 5;\n") {
		t.Fatalf("static jail.conf:\n%s", s)
	}
	spec.NICs[1].DHCP = true
	conf, _ = renderJailConf(spec, "/r", "/l")
	if !strings.Contains(string(conf), "\tdevfs_ruleset = 6001;\n") {
		t.Fatalf("a DHCP jail does not get ruleset 6001:\n%s", conf)
	}
	for _, bad := range [][]JailNIC{
		{{Slot: 0, Bridge: "bridge0", MAC: "02:00:00:00:00:01"}, {Slot: 0, Bridge: "bridge0", MAC: "02:00:00:00:00:02"}},
		{{Slot: 8, Bridge: "bridge0", MAC: "02:00:00:00:00:01"}},
		{{Slot: 0, Bridge: "bridge0", MAC: "03:00:00:00:00:01"}},
		{{Slot: 0, Bridge: "bridge0", MAC: "02:00:00:00:00:0A"}},
		{{Slot: 0, Bridge: "bridge0\"; exec.prestart=\"x", MAC: "02:00:00:00:00:01"}},
	} {
		spec.NICs = bad
		if _, err := renderJailConf(spec, "/r", "/l"); err == nil {
			t.Errorf("NICs %+v accepted", bad)
		}
	}
}

func TestCheckJailParamsDevfs(t *testing.T) {
	spec := jailSpec()
	spec.NICs = []JailNIC{{Slot: 0, Bridge: "bridge0", MAC: "02:00:00:00:00:01", DHCP: true}}
	params := goodParams("/r")
	if err := checkJailParams(params, spec, "/r"); err == nil || !strings.Contains(err.Error(), "devfs_ruleset=5 (want 6001)") {
		t.Fatalf("a DHCP jail on ruleset 5 passed: %v", err)
	}
	params["devfs_ruleset"] = "6001"
	if err := checkJailParams(params, spec, "/r"); err != nil {
		t.Fatal(err)
	}
}

const showRuleset = "devfs rule -s 6001 show"

// ruleset6001 is ruleset 6001 as devfs rule show prints it (amendment i).
const ruleset6001 = "100 include 1\n200 include 2\n300 include 3\n400 include 4\n500 path pf unhide\n600 path bpf* unhide\n"

func TestEnsureDHCPRuleset(t *testing.T) {
	adds := []string{
		"devfs rule -s 6001 add 100 include 1",
		"devfs rule -s 6001 add 200 include 2",
		"devfs rule -s 6001 add 300 include 3",
		"devfs rule -s 6001 add 400 include 4",
		"devfs rule -s 6001 add 500 path pf unhide",
		"devfs rule -s 6001 add 600 path bpf* unhide",
	}
	// Absent: added in order, then checked. Never an include of 5: devfs
	// does not resolve the includes of an included ruleset.
	h, run, _ := jailHost(t)
	run.answers[showRuleset] = []any{"", ruleset6001}
	if err := h.ensureDHCPRuleset(context.Background()); err != nil {
		t.Fatal(err)
	}
	var added []string
	for _, c := range run.calls {
		if strings.Contains(c, " add ") {
			added = append(added, c)
		}
	}
	if !slices.Equal(added, adds) {
		t.Fatalf("added %q, want %q", added, adds)
	}
	// Present: nothing added.
	h, run, _ = jailHost(t)
	run.answers[showRuleset] = ruleset6001
	if err := h.ensureDHCPRuleset(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range run.calls {
		if strings.Contains(c, " add ") {
			t.Fatalf("added again: %q", run.calls)
		}
	}
	// Someone else's, amendment h's broken one included: refused, nothing
	// added or removed.
	for _, foreign := range []string{"100 path pf unhide\n", "100 include 5\n200 path bpf* unhide\n",
		ruleset6001 + "700 path mem unhide\n"} {
		h, run, _ = jailHost(t)
		run.answers[showRuleset] = foreign
		wantCode(t, h.ensureDHCPRuleset(context.Background()), models.CodeDevfsRulesetConflict)
		for _, c := range run.calls {
			if c != showRuleset {
				t.Fatalf("ran %q on a foreign ruleset", c)
			}
		}
	}
}

func TestEpairAvailable(t *testing.T) {
	h, run, _ := jailHost(t)
	run.answers["kldstat -q -m if_epair"] = &CommandError{Name: "kldstat", ExitCode: 1, Err: errors.New("exit status 1")}
	wantCode(t, h.EpairAvailable(context.Background()), models.CodeEpairUnavailable)
	h, _, _ = jailHost(t)
	if err := h.EpairAvailable(context.Background()); err != nil {
		t.Fatal(err)
	}
}

const epairCreate = "ifconfig epair create inet6 description skep:" + ctrA + ":0 ifdisabled -auto_linklocal up"

func TestCreateJailEpair(t *testing.T) {
	h, run, _ := jailHost(t)
	run.answers["ifconfig sjc0c0c0c011n0"] = noSuchInterface("sjc0c0c0c011n0")
	run.answers[epairCreate] = "epair3a\n"
	host, err := h.createJailEpair(context.Background(), ctrA, JailNIC{Slot: 0, Bridge: "bridge0", MAC: "02:5b:00:00:00:01", DHCP: true})
	if err != nil || host != "epair3a" {
		t.Fatalf("createJailEpair = %q, %v", host, err)
	}
	want := []string{
		"ifconfig sjc0c0c0c011n0", "ifconfig -g epair", epairCreate,
		"ifconfig epair3b name sjc0c0c0c011n0", "ifconfig sjc0c0c0c011n0 ether 02:5b:00:00:00:01", "ifconfig bridge0 addm epair3a",
	}
	if !slices.Equal(run.calls, want) {
		t.Fatalf("calls:\n%q\nwant\n%q", run.calls, want)
	}
}

func TestCreateJailEpairRefusesForeignName(t *testing.T) {
	h, run, _ := jailHost(t)
	run.answers["ifconfig sjc0c0c0c011n0"] = ifconfigShow("sjc0c0c0c011n0", "")
	if _, err := h.createJailEpair(context.Background(), ctrA, JailNIC{Slot: 0, Bridge: "bridge0", MAC: "02:5b:00:00:00:01"}); err == nil {
		t.Fatal("an existing interface of the jail end's name was ignored")
	}
	for _, c := range run.calls {
		if strings.Contains(c, "create") {
			t.Fatalf("created anyway: %q", c)
		}
	}
}

func TestEpairCreateFailureDestroysOnlyOurs(t *testing.T) {
	h, run, _ := jailHost(t)
	desc := "skep:" + ctrA + ":0"
	run.answers["ifconfig -g epair"] = []any{"epair0b\nepair0a\n", "epair0b\nepair0a\nepair1b\nepair1a\nepair2b\nepair2a\nepair3b\nepair3a\n"}
	run.answers[epairCreate] = &CommandError{Name: "ifconfig", ExitCode: 1, Stderr: "ifconfig: inet6: bad value", Err: errors.New("exit status 1")}
	run.answers["ifconfig epair1a"] = ifconfigShow("epair1a", desc)
	run.answers["ifconfig epair2a"] = ifconfigShow("epair2a", "")
	run.answers["ifconfig epair3a"] = ifconfigShow("epair3a", "skep:"+ctrB+":0")
	if _, err := h.epairCreate(context.Background(), desc); err == nil {
		t.Fatal("a failed create succeeded")
	}
	var destroyed []string
	for _, c := range run.calls {
		if strings.HasSuffix(c, " destroy") {
			destroyed = append(destroyed, c)
		}
	}
	if !slices.Equal(destroyed, []string{"ifconfig epair1a destroy"}) {
		t.Fatalf("destroyed %q; only the new epair with this description may go", destroyed)
	}
}

func TestDestroyJailInterfaces(t *testing.T) {
	h, run, _ := jailHost(t)
	run.answers["ifconfig -g epair"] = "epair0b\nepair0a\nepair1a\nepair1b\nepair2a\nsjc0c0c0c011n0\n"
	run.answers["ifconfig epair0a"] = ifconfigShow("epair0a", "skep:"+ctrA+":0")
	run.answers["ifconfig epair1a"] = ifconfigShow("epair1a", "skep:"+ctrB+":0")
	run.answers["ifconfig epair2a"] = ifconfigShow("epair2a", "")
	got, err := h.DestroyJailInterfaces(context.Background(), ctrA)
	if err != nil || !slices.Equal(got, []string{"epair0a"}) {
		t.Fatalf("DestroyJailInterfaces = %q, %v", got, err)
	}
	for _, c := range run.calls {
		if strings.HasSuffix(c, " destroy") && c != "ifconfig epair0a destroy" {
			t.Fatalf("destroyed %q", c)
		}
	}
}

func TestSkepInterfaces(t *testing.T) {
	h, run, _ := jailHost(t)
	run.answers["ifconfig -g epair"] = "epair0a\nepair0b\nepair1a\n"
	run.answers["ifconfig -g tap"] = "tap0\ntap1\n"
	run.answers["ifconfig epair0a"] = ifconfigShow("epair0a", "skep:"+ctrA+":2")
	run.answers["ifconfig epair1a"] = ifconfigShow("epair1a", "mine")
	run.answers["ifconfig tap0"] = ifconfigShow("tap0", "skep:"+vmA+":0")
	run.answers["ifconfig tap1"] = noSuchInterface("tap1")
	got, err := h.SkepInterfaces(context.Background())
	want := []SkepInterface{{Name: "epair0a", Kind: "epair", OwnerID: ctrA, Slot: 2}, {Name: "tap0", Kind: "tap", OwnerID: vmA, Slot: 0}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("SkepInterfaces = %+v, %v", got, err)
	}
}

func TestStopJailDestroysEpairsWhenNotRunning(t *testing.T) {
	h, run, _ := jailHost(t)
	run.answers["jls -j skep-"+ctrA+" jid"] = &CommandError{Name: "jls", ExitCode: 1, Stderr: "jls: jail \"skep-" + ctrA + "\" not found", Err: errors.New("exit status 1")}
	run.answers["ifconfig -g epair"] = "epair0a\nepair0b\n"
	run.answers["ifconfig epair0a"] = ifconfigShow("epair0a", "skep:"+ctrA+":0")
	if err := h.StopJail(context.Background(), jailSpec()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(run.calls, "ifconfig epair0a destroy") {
		t.Fatalf("calls: %q", run.calls)
	}
}

func TestCheckJailNetRefusesWithoutEpair(t *testing.T) {
	// StartJail runs checkJailNet before it creates anything; as a
	// non-root user its parent check refuses earlier, so the check is
	// tested on its own here and through StartJail on hearst.
	h, run, _ := jailHost(t)
	run.answers["kldstat -q -m if_epair"] = &CommandError{Name: "kldstat", ExitCode: 1, Err: errors.New("exit status 1")}
	spec := jailSpec()
	spec.NICs = []JailNIC{{Slot: 0, Bridge: "bridge0", MAC: "02:00:00:00:00:01", DHCP: true}}
	wantCode(t, h.checkJailNet(context.Background(), spec), models.CodeEpairUnavailable)
	for _, c := range run.calls {
		if strings.Contains(c, "epair create") || strings.HasPrefix(c, "devfs") {
			t.Fatalf("ran %q without if_epair", c)
		}
	}
	// A bridge with the management interface is refused before devfs.
	h, run, _ = jailHost(t)
	run.answers["ifconfig bridge0"] = "bridge0: flags=8843<UP> mtu 1500\n\tmember: vtnet0 flags=143<LEARNING>\n"
	wantCode(t, h.checkJailNet(context.Background(), spec), models.CodeBridgeNotAllowed)
	for _, c := range run.calls {
		if strings.HasPrefix(c, "devfs") {
			t.Fatalf("ran %q for a refused bridge", c)
		}
	}
	// Static only: no devfs ruleset is touched.
	h, run, _ = jailHost(t)
	run.answers["ifconfig bridge0"] = "bridge0: flags=8843<UP> mtu 1500\n\tmember: vtnet1 flags=143<LEARNING>\n"
	spec.NICs[0].DHCP = false
	if err := h.checkJailNet(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	for _, c := range run.calls {
		if strings.HasPrefix(c, "devfs") {
			t.Fatalf("a static jail touched devfs: %q", c)
		}
	}
}

func TestJailFilesNameservers(t *testing.T) {
	c, err := JailFiles{Nameservers: []string{"9.9.9.9", "2620:fe::fe"}}.render()
	if err != nil || string(c.resolvConf) != RCConfHeader+"nameserver 9.9.9.9\nnameserver 2620:fe::fe\n" {
		t.Fatalf("render = %q, %v", c.resolvConf, err)
	}
	for _, bad := range [][]string{{"9.9.9.9\nnameserver 1.1.1.1"}, {"fe80::1%em0"}, {"x"}, {"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4"}} {
		if _, err := (JailFiles{Nameservers: bad}).render(); err == nil {
			t.Errorf("nameservers %q accepted", bad)
		}
	}
}

func TestGuestInterfaceName(t *testing.T) {
	for slot, want := range map[int]string{0: "skep0", 1: "skep1", 7: "skep7"} {
		if got, err := GuestInterfaceName(slot); err != nil || got != want {
			t.Fatalf("GuestInterfaceName(%d) = %q, %v", slot, got, err)
		}
	}
	for _, slot := range []int{-1, 8} {
		if _, err := GuestInterfaceName(slot); err == nil {
			t.Fatalf("slot %d accepted", slot)
		}
	}
}

// After a jail is removed outside Skep its jail ends return named skep0,
// two of them alike (the probe of amendment k on hearst). Only the host
// ends identify the owners.
func TestSkepInterfacesIgnoresReturnedGuestNames(t *testing.T) {
	h, run, _ := jailHost(t)
	run.answers["ifconfig -g epair"] = "epair0a\nepair1a\nskep0\nskep0\n"
	run.answers["ifconfig -g tap"] = ""
	run.answers["ifconfig epair0a"] = ifconfigShow("epair0a", "skep:"+ctrA+":0")
	run.answers["ifconfig epair1a"] = ifconfigShow("epair1a", "skep:"+ctrB+":0")
	got, err := h.SkepInterfaces(context.Background())
	want := []SkepInterface{{Name: "epair0a", Kind: "epair", OwnerID: ctrA, Slot: 0}, {Name: "epair1a", Kind: "epair", OwnerID: ctrB, Slot: 0}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("SkepInterfaces = %+v, %v", got, err)
	}
	if slices.Contains(run.calls, "ifconfig skep0") {
		t.Fatalf("read a returned jail end by its ambiguous name: %q", run.calls)
	}
	if _, err := h.DestroyJailInterfaces(context.Background(), ctrA); err != nil {
		t.Fatal(err)
	}
	for _, c := range run.calls {
		if strings.HasSuffix(c, " destroy") && c != "ifconfig epair0a destroy" {
			t.Fatalf("destroyed %q", c)
		}
	}
}
