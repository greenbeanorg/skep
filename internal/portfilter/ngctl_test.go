package portfilter

import (
	"strings"
	"testing"
)

// Output captured on hearst (FreeBSD 15.1-RELEASE-p7) in the M7c probe,
// tests/integration/jails/results/m7c-probe-netgraph-hearst.txt.
const (
	showFiltered = `  Name: epair0a         Type: ether           ID: 00000005   Num hooks: 2
  Local hook      Peer name       Peer type    Peer ID         Peer hook
  ----------      ---------       ---------    -------         ---------
  upper           skf_epair0a     bpf          00000019        bridge
  lower           skf_epair0a     bpf          00000019        jail
`
	showBare = `  Name: epair0a         Type: ether           ID: 00000005   Num hooks: 0
`
	getJail23 = `Rec'd response "getprogram" (2) from "[104]:":
Args:	{ thisHook="jail" ifMatch="bridge" bpf_prog_len=23 bpf_prog=[ { code=0x20 k=6 } { code=0x15 jf=20 k=39714826 } { code=0x28 k=10 } { code=0x15 jf=18 k=16 } { code=0x28 k=12 } { code=0x15 jt=1 k=2048 } { code=0x15 jt=3 jf=15 k=2054 } { code=0x20 k=30 } { code=0x20 k=26 } { code=0x15 jt=11 jf=12 k=3221225994 } { code=0x20 k=14 } { code=0x15 jf=10 k=67584 } { code=0x28 k=18 } { code=0x15 jf=8 k=1540 } { code=0x20 k=22 } { code=0x15 jf=6 k=39714826 } { code=0x28 k=26 } { code=0x15 jf=4 k=16 } { code=0x20 k=28 } { code=0x15 jt=1 k=3221225994 } { code=0x15 jf=1 } { code=0x6 k=4294967295 } { code=0x6 } ] }
`
	getBridge = `Rec'd response "getprogram" (2) from "[104]:":
Args:	{ thisHook="bridge" ifMatch="jail" bpf_prog_len=1 bpf_prog=[ { code=0x6 k=4294967295 } ] }
`
	getNoProgram = `Rec'd response "getprogram" (2) from "[104]:":
Args:	{ thisHook="jail" bpf_prog_len=1 bpf_prog=[ { code=0x6 } ] }
`
)

func TestParseShow(t *testing.T) {
	n, err := ParseShow([]byte(showFiltered))
	if err != nil {
		t.Fatal(err)
	}
	if n.Name != "epair0a" || n.Type != "ether" || n.ID != "5" || len(n.Hooks) != 2 {
		t.Fatalf("node = %+v", n)
	}
	l, ok := n.Hook("lower")
	if !ok || l.PeerType != "bpf" || l.PeerID != "19" || l.PeerHook != "jail" || l.PeerName != "skf_epair0a" {
		t.Errorf("lower = %+v", l)
	}
	if n, err = ParseShow([]byte(showBare)); err != nil || len(n.Hooks) != 0 || n.ID != "5" {
		t.Errorf("bare: %+v, %v", n, err)
	}
	for _, bad := range []string{
		"",
		"ngctl: send msg: No such file or directory\n",
		strings.Replace(showFiltered, "Num hooks: 2", "Num hooks: 3", 1),
		strings.Replace(showFiltered, "00000019        jail", "0000zz19        jail", 1),
		showFiltered + showBare,
	} {
		if _, err := ParseShow([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestParseGetProgram(t *testing.T) {
	hp, err := ParseGetProgram([]byte(getJail23))
	if err != nil {
		t.Fatal(err)
	}
	if hp.ThisHook != "jail" || hp.IfMatch != "bridge" || hp.IfNotMatch != "" || len(hp.Prog) != 23 {
		t.Fatalf("%+v", hp)
	}
	if hp.Prog[1] != (Insn{Code: 0x15, Jf: 20, K: 39714826}) || hp.Prog[22] != (Insn{Code: 6}) {
		t.Errorf("instructions: %+v %+v", hp.Prog[1], hp.Prog[22])
	}
	b, err := ParseGetProgram([]byte(getBridge))
	if err != nil || !Equal(b.Prog, PassAll) || b.IfMatch != "jail" {
		t.Errorf("bridge: %+v, %v", b, err)
	}
	if n, err := ParseGetProgram([]byte(getNoProgram)); err != nil || n.IfMatch != "" || len(n.Prog) != 1 {
		t.Errorf("no program: %+v, %v", n, err)
	}
	for _, bad := range []string{
		"",
		"ngctl: send msg: No such file or directory\n",
		strings.Replace(getBridge, "bpf_prog_len=1", "bpf_prog_len=2", 1),
		strings.Replace(getBridge, "k=4294967295", "k=99999999999", 1),
		strings.Replace(getBridge, "code=0x6", "code=0x6 zz=1", 1),
	} {
		if _, err := ParseGetProgram([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// TestSetProgramRoundTrip: what SetProgram writes is what getprogram's
// parser reads back, for every mode.
func TestSetProgramRoundTrip(t *testing.T) {
	for _, m := range modes {
		p, _ := Program(m.p)
		reply := "Args:\t" + SetProgram(HookJail, HookBridge, "", p) + "\n"
		hp, err := ParseGetProgram([]byte(reply))
		if err != nil || !Equal(hp.Prog, p) {
			t.Errorf("%s: %v", m.name, err)
		}
	}
}

func TestInstallScript(t *testing.T) {
	p, _ := Program(modes[0].p)
	s, err := InstallScript("5", p)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(s), "\n")
	want := []string{
		"mkpeer [5]: bpf lower jail",
		"connect [5]: [5]:lower upper bridge",
		`msg [5]:lower setprogram { thisHook="jail" ifMatch="bridge" ifNotMatch=""`,
		`msg [5]:lower setprogram { thisHook="bridge" ifMatch="jail" ifNotMatch="" bpf_prog_len=1 bpf_prog=[ { code=6 jt=0 jf=0 k=4294967295 } ] }`,
	}
	if len(lines) != len(want) {
		t.Fatalf("%d lines:\n%s", len(lines), s)
	}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w) {
			t.Errorf("line %d:\n got %s\nwant %s...", i, lines[i], w)
		}
	}
	for _, bad := range []NodeID{"", "5]: x", "zz", "123456789"} {
		if _, err := InstallScript(bad, p); err == nil {
			t.Errorf("node ID %q accepted", bad)
		}
	}
}

func TestValidInterface(t *testing.T) {
	for _, ok := range []string{"epair0a", "epair12345a", "tap0", "tap99"} {
		if !ValidInterface(ok) {
			t.Errorf("%s refused", ok)
		}
	}
	for _, bad := range []string{"epair0b", "vtnet0", "bridge0", "tap", "epair0a:", "tap1 lower", ""} {
		if ValidInterface(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestProblem(t *testing.T) {
	prog, _ := Program(modes[0].p)
	good := Node{Name: "epair0a", Type: "ether", ID: "5", Hooks: []Hook{
		{Local: "upper", PeerType: "bpf", PeerID: "19", PeerHook: "bridge"},
		{Local: "lower", PeerType: "bpf", PeerID: "19", PeerHook: "jail"},
	}}
	jail := &HookProgram{ThisHook: "jail", IfMatch: "bridge", Prog: prog}
	bridge := &HookProgram{ThisHook: "bridge", IfMatch: "jail", Prog: PassAll}
	if p := Problem(good, "5", jail, bridge, prog); p != "" {
		t.Fatalf("complete filter: %q", p)
	}
	only := func(hooks ...Hook) Node { n := good; n.Hooks = hooks; return n }
	other, _ := Program(modes[1].p)
	cases := map[string]string{
		"node changed":   Problem(good, "6", jail, bridge, prog),
		"no hooks":       Problem(only(), "5", nil, nil, prog),
		"lower removed":  Problem(only(good.Hooks[0]), "5", nil, bridge, prog),
		"upper removed":  Problem(only(good.Hooks[1]), "5", jail, nil, prog),
		"foreign lower":  Problem(only(good.Hooks[0], Hook{Local: "lower", PeerType: "socket", PeerID: "30", PeerHook: "x"}), "5", nil, bridge, prog),
		"split nodes":    Problem(only(good.Hooks[0], Hook{Local: "lower", PeerType: "bpf", PeerID: "20", PeerHook: "jail"}), "5", jail, bridge, prog),
		"pass-all":       Problem(good, "5", &HookProgram{ThisHook: "jail", IfMatch: "bridge", Prog: PassAll}, bridge, prog),
		"other program":  Problem(good, "5", &HookProgram{ThisHook: "jail", IfMatch: "bridge", Prog: other}, bridge, prog),
		"no program":     Problem(good, "5", &HookProgram{ThisHook: "jail", Prog: []Insn{{Code: opRetK}}}, bridge, prog),
		"unreadable":     Problem(good, "5", nil, bridge, prog),
		"bridge program": Problem(good, "5", jail, &HookProgram{ThisHook: "bridge", IfMatch: "jail", Prog: []Insn{{Code: opRetK}}}, prog),
		"extra hook":     Problem(only(append(good.Hooks, Hook{Local: "orphans", PeerType: "socket", PeerID: "40", PeerHook: "x"})...), "5", jail, bridge, prog),
	}
	for name, p := range cases {
		if p == "" {
			t.Errorf("%s: reported as complete", name)
		}
	}
}
