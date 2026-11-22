package portfilter

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Hook names on the ng_bpf node, and the ng_ether hooks they connect to.
const (
	HookJail   = "jail"
	HookBridge = "bridge"
	EtherLower = "lower"
	EtherUpper = "upper"
)

// NodePrefix starts the name of a port's ng_bpf node: skf_<interface>. The
// name is for operators reading "ngctl list"; nothing Skep does depends on
// it, because names can collide and IDs cannot.
const NodePrefix = "skf_"

var (
	ifNameRE = regexp.MustCompile(`^(epair[0-9]{1,5}a|tap[0-9]{1,5})$`)
	nodeIDRE = regexp.MustCompile(`^[0-9a-f]{1,8}$`)
)

// ValidInterface reports whether name is a guest port Skep filters: an
// epair host end or a tap, as the kernel names them.
func ValidInterface(name string) bool { return ifNameRE.MatchString(name) }

// NodeID is a netgraph node ID as ngctl prints it, in hex.
type NodeID string

// Valid reports whether id is a node ID.
func (id NodeID) Valid() bool { return nodeIDRE.MatchString(string(id)) }

// Addr is the netgraph address of the node: [id]:.
func (id NodeID) Addr() string { return "[" + string(id) + "]:" }

// NodeName is the name given to a port's ng_bpf node.
func NodeName(ifName string) string { return NodePrefix + ifName }

// SetProgram renders a "setprogram" argument (struct ng_bpf_hookprog in
// ng_parse ASCII form).
func SetProgram(this, match, nomatch string, p []Insn) string {
	var b strings.Builder
	fmt.Fprintf(&b, "{ thisHook=%q ifMatch=%q ifNotMatch=%q bpf_prog_len=%d bpf_prog=[", this, match, nomatch, len(p))
	for _, in := range p {
		fmt.Fprintf(&b, " { code=%d jt=%d jf=%d k=%d }", in.Code, in.Jt, in.Jf, in.K)
	}
	b.WriteString(" ] }")
	return b.String()
}

// InstallScript returns the ngctl -f script that puts the filter on the
// ether node ether, addressed by ID so that a replaced interface of the
// same name is never reached.
//
// Order matters for failing closed: the "jail" hook is connected to lower
// first. A new ng_bpf hook drops everything until its program is set, so
// from that moment until the real program is loaded, frames from the
// guest are dropped, never passed. The "bridge" hook is connected next;
// programs are loaded last. ngctl -f stops at the first failing line.
func InstallScript(ether NodeID, prog []Insn) (string, error) {
	if !ether.Valid() {
		return "", fmt.Errorf("portfilter: invalid node ID %q", ether)
	}
	bpf := ether.Addr() + EtherLower
	var b strings.Builder
	fmt.Fprintf(&b, "mkpeer %s bpf %s %s\n", ether.Addr(), EtherLower, HookJail)
	fmt.Fprintf(&b, "connect %s %s %s %s\n", ether.Addr(), bpf, EtherUpper, HookBridge)
	fmt.Fprintf(&b, "msg %s setprogram %s\n", bpf, SetProgram(HookJail, HookBridge, "", prog))
	fmt.Fprintf(&b, "msg %s setprogram %s\n", bpf, SetProgram(HookBridge, HookJail, "", PassAll))
	return b.String(), nil
}

// Hook is one row of "ngctl show": a hook of the node and its peer.
type Hook struct {
	Local    string
	PeerName string
	PeerType string
	PeerID   NodeID
	PeerHook string
}

// Node is what "ngctl show" prints about a node.
type Node struct {
	Name  string
	Type  string
	ID    NodeID
	Hooks []Hook
}

// Hook returns the node's hook of that name.
func (n Node) Hook(local string) (Hook, bool) {
	for _, h := range n.Hooks {
		if h.Local == local {
			return h, true
		}
	}
	return Hook{}, false
}

// ParseShow reads the output of "ngctl show <path>" on FreeBSD 15.1:
//
//	Name: epair0a         Type: ether           ID: 00000005   Num hooks: 2
//	Local hook      Peer name       Peer type    Peer ID         Peer hook
//	----------      ---------       ---------    -------         ---------
//	upper           skf_epair0a     bpf          00000019        bridge
//	lower           skf_epair0a     bpf          00000019        jail
func ParseShow(out []byte) (Node, error) {
	var n Node
	lines := strings.Split(string(out), "\n")
	header := false
	want := -1
	for _, line := range lines {
		f := strings.Fields(line)
		switch {
		case len(f) == 0:
		case f[0] == "Name:":
			if header || len(f) != 9 || f[2] != "Type:" || f[4] != "ID:" || f[6] != "Num" || f[7] != "hooks:" {
				return Node{}, fmt.Errorf("portfilter: unexpected ngctl show header %q", clean(line))
			}
			header = true
			n.Name, n.Type, n.ID = f[1], f[3], NodeID(strings.ToLower(strings.TrimLeft(f[5], "0")))
			if n.ID == "" {
				n.ID = "0"
			}
			var err error
			if want, err = strconv.Atoi(f[8]); err != nil || want < 0 || !n.ID.Valid() {
				return Node{}, fmt.Errorf("portfilter: unexpected ngctl show header %q", clean(line))
			}
		case f[0] == "Local" || strings.HasPrefix(f[0], "---"):
		default:
			if !header || len(f) != 5 {
				return Node{}, fmt.Errorf("portfilter: unexpected ngctl show line %q", clean(line))
			}
			id := NodeID(strings.ToLower(strings.TrimLeft(f[3], "0")))
			if id == "" {
				id = "0"
			}
			if !id.Valid() {
				return Node{}, fmt.Errorf("portfilter: unexpected peer ID in %q", clean(line))
			}
			n.Hooks = append(n.Hooks, Hook{Local: f[0], PeerName: f[1], PeerType: f[2], PeerID: id, PeerHook: f[4]})
		}
	}
	if !header {
		return Node{}, fmt.Errorf("portfilter: no node in the ngctl show output")
	}
	if len(n.Hooks) != want {
		return Node{}, fmt.Errorf("portfilter: ngctl show announced %d hooks and listed %d", want, len(n.Hooks))
	}
	return n, nil
}

// HookProgram is a parsed "getprogram" reply.
type HookProgram struct {
	ThisHook, IfMatch, IfNotMatch string
	Prog                          []Insn
}

// ParseGetProgram reads the output of "ngctl msg <bpf> getprogram
// '"<hook>"'" on FreeBSD 15.1. ng_parse leaves out fields that are zero
// or empty and prints code in hex:
//
//	Rec'd response "getprogram" (2) from "[104]:":
//	Args:	{ thisHook="jail" ifMatch="bridge" bpf_prog_len=2 bpf_prog=[ { code=0x15 jf=20 k=39714826 } { code=0x6 } ] }
func ParseGetProgram(out []byte) (HookProgram, error) {
	var args string
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Args:"); ok {
			args = strings.TrimSpace(rest)
		}
	}
	if args == "" {
		return HookProgram{}, fmt.Errorf("portfilter: no Args in the getprogram reply")
	}
	open := strings.Index(args, "bpf_prog=[")
	end := strings.LastIndex(args, "]")
	if !strings.HasPrefix(args, "{") || !strings.HasSuffix(args, "}") || open < 0 || end < open {
		return HookProgram{}, fmt.Errorf("portfilter: unexpected getprogram reply %q", clean(args))
	}
	var hp HookProgram
	length := 0
	for _, tok := range strings.Fields(args[1:open]) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			return HookProgram{}, fmt.Errorf("portfilter: unexpected field %q in a getprogram reply", clean(tok))
		}
		switch k {
		case "thisHook", "ifMatch", "ifNotMatch":
			s, err := strconv.Unquote(v)
			if err != nil {
				return HookProgram{}, fmt.Errorf("portfilter: unexpected %s in a getprogram reply", k)
			}
			switch k {
			case "thisHook":
				hp.ThisHook = s
			case "ifMatch":
				hp.IfMatch = s
			default:
				hp.IfNotMatch = s
			}
		case "bpf_prog_len":
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 || n > MaxInsns {
				return HookProgram{}, fmt.Errorf("portfilter: unexpected bpf_prog_len %q", clean(v))
			}
			length = n
		default:
			return HookProgram{}, fmt.Errorf("portfilter: unexpected field %q in a getprogram reply", clean(k))
		}
	}
	body := strings.TrimSpace(args[open+len("bpf_prog=[") : end])
	for body != "" {
		if !strings.HasPrefix(body, "{") {
			return HookProgram{}, fmt.Errorf("portfilter: unexpected instruction list in a getprogram reply")
		}
		close := strings.Index(body, "}")
		if close < 0 {
			return HookProgram{}, fmt.Errorf("portfilter: unterminated instruction in a getprogram reply")
		}
		in, err := parseInsn(body[1:close])
		if err != nil {
			return HookProgram{}, err
		}
		hp.Prog = append(hp.Prog, in)
		body = strings.TrimSpace(body[close+1:])
	}
	if len(hp.Prog) != length {
		return HookProgram{}, fmt.Errorf("portfilter: getprogram announced %d instructions and listed %d", length, len(hp.Prog))
	}
	return hp, nil
}

func parseInsn(s string) (Insn, error) {
	var in Insn
	for _, tok := range strings.Fields(s) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			return Insn{}, fmt.Errorf("portfilter: unexpected instruction field %q", clean(tok))
		}
		n, err := strconv.ParseUint(v, 0, 32)
		if err != nil {
			return Insn{}, fmt.Errorf("portfilter: unexpected instruction value %q", clean(tok))
		}
		switch k {
		case "code":
			if n > 0xffff {
				return Insn{}, fmt.Errorf("portfilter: unexpected opcode %q", clean(v))
			}
			in.Code = uint16(n)
		case "jt", "jf":
			if n > 0xff {
				return Insn{}, fmt.Errorf("portfilter: unexpected jump %q", clean(tok))
			}
			if k == "jt" {
				in.Jt = uint8(n)
			} else {
				in.Jf = uint8(n)
			}
		case "k":
			in.K = uint32(n)
		default:
			return Insn{}, fmt.Errorf("portfilter: unexpected instruction field %q", clean(k))
		}
	}
	return in, nil
}

// clean shortens and strips text for an error message.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// Problem says what is wrong with a port's filter, or "" when it is
// complete: the ether node is the one expected (want, when set), exactly
// "lower" and "upper" are connected, both to the same ng_bpf node, on its
// "jail" and "bridge" hooks, and the two programs are the expected ones.
// jail and bridge are the getprogram replies, nil when they could not be
// read.
func Problem(ether Node, want NodeID, jail, bridge *HookProgram, prog []Insn) string {
	if ether.Type != "ether" {
		return "the interface's netgraph node is not an ether node"
	}
	if want != "" && ether.ID != want {
		return "the interface's netgraph node changed"
	}
	lower, okL := ether.Hook(EtherLower)
	upper, okU := ether.Hook(EtherUpper)
	switch {
	case !okL && !okU:
		return "no filter: the lower and upper hooks are not connected"
	case !okL:
		return "the lower hook is not connected: frames from the guest bypass the filter"
	case !okU:
		return "the upper hook is not connected"
	case len(ether.Hooks) != 2:
		return "the interface's netgraph node has hooks other than lower and upper"
	case lower.PeerType != "bpf" || lower.PeerHook != HookJail:
		return "the lower hook is not connected to a filter's jail hook"
	case upper.PeerType != "bpf" || upper.PeerHook != HookBridge:
		return "the upper hook is not connected to a filter's bridge hook"
	case lower.PeerID != upper.PeerID:
		return "lower and upper are connected to different nodes"
	}
	if jail == nil {
		return "the filter's program could not be read"
	}
	if jail.ThisHook != HookJail || jail.IfMatch != HookBridge || jail.IfNotMatch != "" || !Equal(jail.Prog, prog) {
		return "the filter's program is not the one generated for this port"
	}
	if bridge == nil || bridge.ThisHook != HookBridge || bridge.IfMatch != HookJail || bridge.IfNotMatch != "" || !Equal(bridge.Prog, PassAll) {
		return "the filter's bridge-side program is not the pass-all"
	}
	return ""
}
