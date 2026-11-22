package hostops

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/greenbeanorg/skep/internal/portfilter"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Guest-port anti-spoofing (ADR 0011). Every guest port (a jail's epair
// host end, a VM's tap) gets an ng_bpf filter between its ng_ether node's
// lower and upper hooks before it joins its bridge. The program comes from
// internal/portfilter; this file runs ngctl(8) with validated arguments
// and a script on standard input, never through a shell.

// ngctlBinary talks to netgraph.
const ngctlBinary = "/usr/sbin/ngctl"

// PortFilterModules are the kernel modules the filters need, by module
// name for kldstat -m. Skep loads none of them (ADR 0009 decision 6b): an
// mkpeer of an unloaded node type would load it as a side effect for
// root, so the check comes first.
var PortFilterModules = []string{"netgraph", "ng_socket", "ng_ether", "ng_bpf"}

// PortFilters installs, inspects, and repairs the filters of guest ports.
type PortFilters interface {
	// PortFilterAvailable refuses with port_filter_unavailable when a
	// netgraph module the filters need is not loaded.
	PortFilterAvailable(ctx context.Context) error
	// InstallPortFilter puts the filter for p on ifName, a port that has
	// just been created and is not a bridge member yet, and verifies it.
	// It returns the ether node's ID. Any failure is port_filter_failed.
	InstallPortFilter(ctx context.Context, ifName string, p portfilter.Params) (portfilter.NodeID, error)
	// InspectPortFilter reads ifName's filter and compares it with the
	// one generated for p.
	InspectPortFilter(ctx context.Context, ifName string, p portfilter.Params) (PortFilterStatus, error)
	// ReinstallPortFilter replaces what is left of the filter on the
	// ether node ether (the ID InspectPortFilter returned for ifName):
	// hooks of lower and upper whose peer is an ng_bpf node are removed,
	// hooks to anything else are left alone (and make the install fail),
	// then the filter is installed and verified, all by node ID.
	ReinstallPortFilter(ctx context.Context, ifName string, ether portfilter.NodeID, p portfilter.Params) error
	// DetachPort removes ifName from bridge (deletem), so a guest whose
	// filter cannot be restored is cut off rather than unfiltered.
	DetachPort(ctx context.Context, bridge, ifName string) error
	// LockPort takes the lock of an owner's NIC slot (ADR 0011 addition
	// a). With wait false it fails at once with ErrPortBusy when another
	// holder has it. unlock releases it.
	LockPort(kind PortOwner, ownerID string, slot int, wait bool) (unlock func(), err error)
	// BridgeVLAN reads a bridge's VLAN filtering state.
	BridgeVLAN(ctx context.Context, bridge string) (BridgeVLAN, error)
}

// PortOwner says whose port it is.
type PortOwner string

// Port owners.
const (
	PortJail PortOwner = "jail"
	PortVM   PortOwner = "vm"
)

// ErrPortBusy is LockPort's answer when the lock is held and wait is
// false: an install of that port is running.
var ErrPortBusy = errors.New("hostops: the port is locked by an install in progress")

// PortFilterStatus is what InspectPortFilter found.
type PortFilterStatus struct {
	// Gone is true when the interface or its netgraph node does not
	// exist (any more).
	Gone bool
	// Ether is the interface's ng_ether node ID.
	Ether portfilter.NodeID
	// Problem is "" when the filter is complete and is the one generated
	// for the port; otherwise what is wrong.
	Problem string
}

// BridgeVLAN is a bridge's VLAN filtering state (if_bridge vlanfilter,
// FreeBSD 15).
type BridgeVLAN struct {
	Exists bool
	// Filtering is the bridge flag VLANFILTER.
	Filtering bool
	// PVID maps each member to its untagged VLAN (ifuntagged); a member
	// without one is absent.
	PVID map[string]int
}

func portFilterFailed(ifName, step string, err error) error {
	return &models.Error{Code: models.CodePortFilterFailed,
		Message: fmt.Sprintf("the anti-spoofing filter could not be installed on %s (%s): %v", ifName, step, err),
		Details: map[string]any{"interface": ifName, "step": step}}
}

// PortFilterAvailable implements PortFilters.
func (h *host) PortFilterAvailable(ctx context.Context) error {
	var missing []string
	for _, m := range PortFilterModules {
		if _, err := h.run.Run(ctx, kldstatBinary, "-q", "-m", m); err != nil {
			missing = append(missing, m)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return &models.Error{Code: models.CodePortFilterUnavailable,
		Message: "the netgraph modules " + strings.Join(missing, ", ") + " are not loaded, so a guest port cannot get its anti-spoofing filter; " +
			"the operator loads them (kld_list=\"netgraph ng_socket ng_ether ng_bpf\" in rc.conf); Skep loads no modules",
		Details: map[string]any{"missing_modules": missing}}
}

// ngShow runs ngctl show on a netgraph path. gone is true when the node
// does not exist.
func (h *host) ngShow(ctx context.Context, path string) (node portfilter.Node, gone bool, err error) {
	out, err := h.run.Run(ctx, ngctlBinary, "show", path)
	if err != nil {
		if ngNoSuchNode(err) {
			return portfilter.Node{}, true, nil
		}
		return portfilter.Node{}, false, fmt.Errorf("hostops: ngctl show %s: %w", path, err)
	}
	node, err = portfilter.ParseShow(out)
	return node, false, err
}

// ngNoSuchNode recognises ngctl's answer for a path that leads nowhere.
func ngNoSuchNode(err error) bool {
	return stderrContains(err, "No such file or directory") || stderrContains(err, "not found")
}

// ngProgram reads one hook's program of the ng_bpf node bpf.
func (h *host) ngProgram(ctx context.Context, bpf portfilter.NodeID, hook string) (*portfilter.HookProgram, error) {
	out, err := h.run.Run(ctx, ngctlBinary, "msg", bpf.Addr(), "getprogram", strconv.Quote(hook))
	if err != nil {
		return nil, err
	}
	hp, err := portfilter.ParseGetProgram(out)
	if err != nil {
		return nil, err
	}
	return &hp, nil
}

// inspect compares the filter on the ether node at path (by name or ID)
// with prog.
func (h *host) inspect(ctx context.Context, path string, want portfilter.NodeID, prog []portfilter.Insn) (PortFilterStatus, error) {
	node, gone, err := h.ngShow(ctx, path)
	if err != nil || gone {
		return PortFilterStatus{Gone: gone}, err
	}
	st := PortFilterStatus{Ether: node.ID}
	var jail, bridge *portfilter.HookProgram
	if lower, ok := node.Hook(portfilter.EtherLower); ok && lower.PeerType == "bpf" {
		jail, _ = h.ngProgram(ctx, lower.PeerID, portfilter.HookJail)
		bridge, _ = h.ngProgram(ctx, lower.PeerID, portfilter.HookBridge)
	}
	st.Problem = portfilter.Problem(node, want, jail, bridge, prog)
	return st, nil
}

// runScript runs an ngctl script on standard input.
func (h *host) runScript(ctx context.Context, script string) error {
	er, err := h.envRunner()
	if err != nil {
		return err
	}
	_, err = er.RunEnv(ctx, EnvCommand{Path: ngctlBinary, Args: []string{"-f", "-"}, Stdin: []byte(script),
		Env: []string{"LC_ALL=C", "PATH=/sbin:/bin:/usr/sbin:/usr/bin"}})
	return cleanStderr(err)
}

// installOn runs the install script on the ether node and verifies the
// result by ID.
func (h *host) installOn(ctx context.Context, ifName string, ether portfilter.NodeID, prog []portfilter.Insn) error {
	script, err := portfilter.InstallScript(ether, prog)
	if err != nil {
		return portFilterFailed(ifName, "script", err)
	}
	if err := h.runScript(ctx, script); err != nil {
		return portFilterFailed(ifName, "install", err)
	}
	// The name only helps an operator reading ngctl list; a leftover node
	// of the same name must not fail the install.
	if _, err := h.run.Run(ctx, ngctlBinary, "name", ether.Addr()+portfilter.EtherLower, portfilter.NodeName(ifName)); err != nil {
		h.log.Info("hostops: could not name a port filter node", "interface", ifName, "error", err)
	}
	st, err := h.inspect(ctx, ether.Addr(), ether, prog)
	switch {
	case err != nil:
		return portFilterFailed(ifName, "verify", err)
	case st.Gone:
		return portFilterFailed(ifName, "verify", errors.New("the interface disappeared"))
	case st.Problem != "":
		return portFilterFailed(ifName, "verify", errors.New(st.Problem))
	}
	return nil
}

// InstallPortFilter implements PortFilters.
func (h *host) InstallPortFilter(ctx context.Context, ifName string, p portfilter.Params) (portfilter.NodeID, error) {
	if !portfilter.ValidInterface(ifName) {
		return "", invalid("invalid guest port name")
	}
	prog, err := portfilter.Program(p)
	if err != nil {
		return "", portFilterFailed(ifName, "program", err)
	}
	node, gone, err := h.ngShow(ctx, ifName+":")
	switch {
	case err != nil:
		return "", portFilterFailed(ifName, "inspect", err)
	case gone:
		return "", portFilterFailed(ifName, "inspect", errors.New("the interface has no ng_ether node"))
	case node.Type != "ether":
		return "", portFilterFailed(ifName, "inspect", errors.New("the interface's netgraph node is not an ether node"))
	case len(node.Hooks) != 0:
		// A fresh interface always has free hooks; someone else's graph
		// on Skep's port is not supported (ADR 0011 decision 10).
		return "", portFilterFailed(ifName, "inspect", errors.New("the interface's netgraph hooks are already in use"))
	}
	if err := h.installOn(ctx, ifName, node.ID, prog); err != nil {
		return "", err
	}
	return node.ID, nil
}

// InspectPortFilter implements PortFilters.
func (h *host) InspectPortFilter(ctx context.Context, ifName string, p portfilter.Params) (PortFilterStatus, error) {
	if !portfilter.ValidInterface(ifName) {
		return PortFilterStatus{}, invalid("invalid guest port name")
	}
	prog, err := portfilter.Program(p)
	if err != nil {
		return PortFilterStatus{}, err
	}
	return h.inspect(ctx, ifName+":", "", prog)
}

// ReinstallPortFilter implements PortFilters.
func (h *host) ReinstallPortFilter(ctx context.Context, ifName string, ether portfilter.NodeID, p portfilter.Params) error {
	if !portfilter.ValidInterface(ifName) {
		return invalid("invalid guest port name")
	}
	if !ether.Valid() {
		return invalid("invalid netgraph node ID")
	}
	prog, err := portfilter.Program(p)
	if err != nil {
		return portFilterFailed(ifName, "program", err)
	}
	node, gone, err := h.ngShow(ctx, ether.Addr())
	switch {
	case err != nil:
		return portFilterFailed(ifName, "inspect", err)
	case gone:
		return portFilterFailed(ifName, "inspect", errors.New("the interface's netgraph node is gone"))
	case node.Type != "ether" || node.Name != ifName:
		return portFilterFailed(ifName, "inspect", errors.New("the netgraph node is no longer this interface's"))
	}
	for _, hk := range node.Hooks {
		if (hk.Local == portfilter.EtherLower || hk.Local == portfilter.EtherUpper) && hk.PeerType == "bpf" {
			if _, err := h.run.Run(ctx, ngctlBinary, "rmhook", ether.Addr(), hk.Local); err != nil && !ngNoSuchNode(err) {
				return portFilterFailed(ifName, "remove the old filter", err)
			}
		}
	}
	return h.installOn(ctx, ifName, ether, prog)
}

// DetachPort implements PortFilters.
func (h *host) DetachPort(ctx context.Context, bridge, ifName string) error {
	if !portfilter.ValidInterface(ifName) {
		return invalid("invalid guest port name")
	}
	if !bridgeNameRE.MatchString(bridge) {
		return invalid("invalid bridge name")
	}
	if _, err := h.run.Run(ctx, ifconfigBinary, bridge, "deletem", ifName); err != nil {
		return fmt.Errorf("hostops: remove %s from %s: %w", ifName, bridge, err)
	}
	return nil
}

// BridgeVLAN implements PortFilters.
func (h *host) BridgeVLAN(ctx context.Context, bridge string) (BridgeVLAN, error) {
	if !bridgeNameRE.MatchString(bridge) {
		return BridgeVLAN{}, invalid("invalid bridge name")
	}
	out, err := h.run.Run(ctx, ifconfigBinary, bridge)
	if err != nil {
		if stderrContains(err, "does not exist") {
			return BridgeVLAN{}, nil
		}
		return BridgeVLAN{}, fmt.Errorf("hostops: read %s: %w", bridge, err)
	}
	return parseBridgeVLAN(out), nil
}

// parseBridgeVLAN reads ifconfig <bridge> on FreeBSD 15.1:
//
//	bridge flags=1<VLANFILTER>
//	member: epair0a flags=143<LEARNING,DISCOVER,AUTOEDGE,AUTOPTP>
//	        port 6 priority 128 path cost 2000 vlan protocol 802.1q untagged 1
func parseBridgeVLAN(out []byte) BridgeVLAN {
	b := BridgeVLAN{Exists: true, PVID: map[string]int{}}
	member := ""
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if rest, ok := strings.CutPrefix(line, "bridge flags="); ok {
			_, flags, _ := strings.Cut(rest, "<")
			flags, _, _ = strings.Cut(flags, ">")
			for _, f := range strings.Split(flags, ",") {
				if f == "VLANFILTER" {
					b.Filtering = true
				}
			}
			continue
		}
		if rest, ok := strings.CutPrefix(line, "member: "); ok {
			member, _, _ = strings.Cut(rest, " ")
			continue
		}
		if member != "" && strings.HasPrefix(line, "port ") {
			f := strings.Fields(line)
			for i := 0; i+1 < len(f); i++ {
				if f[i] == "untagged" {
					if n, err := strconv.Atoi(f[i+1]); err == nil && n > 0 && n < 4095 {
						b.PVID[member] = n
					}
				}
			}
			member = ""
		}
	}
	return b
}
