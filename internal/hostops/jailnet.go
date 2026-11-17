package hostops

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/greenbeanorg/skep/pkg/models"
)

// Jail networking (ADR 0010 decision 10, amendment h). Every NIC of a jail
// is an epair: the host end (epairNa) is created, described
// skep:<container-id>:<slot>, and given no address in one ifconfig call,
// then joins an allow-listed bridge; the jail end is renamed on the host
// to a stable name and moved into the jail by jail(8)'s vnet.interface.
// Destroying the host end destroys both ends.

// devfsBinary manages the devfs rulesets.
const devfsBinary = "/sbin/devfs"

// JailDHCPDevfsRuleset is Skep's devfs ruleset for jails with a DHCP
// interface: ruleset 5's rules plus bpf, which dhclient needs (amendments
// h and i). The agent adds it to the kernel at runtime; nothing is written
// to a file.
const JailDHCPDevfsRuleset = 6001

// jailDHCPRules are the rules of JailDHCPDevfsRuleset exactly as
// "devfs rule -s 6001 show" prints them. devfs does not resolve the
// includes of an included ruleset, so "include 5" would hide nothing
// (amendment i): ruleset 5's own rules are written out, with the same
// single level of includes, and bpf added.
var jailDHCPRules = []string{
	"100 include 1",
	"200 include 2",
	"300 include 3",
	"400 include 4",
	"500 path pf unhide",
	"600 path bpf* unhide",
}

// jailIfacePrefix starts the jail end's name: sj, ten hex digits of the
// container ID, n, and the slot. 14 characters at most, under IFNAMSIZ.
const jailIfacePrefix = "sj"

var (
	epairHostRE = regexp.MustCompile(`^epair[0-9]{1,5}a$`)
	jailIfaceRE = regexp.MustCompile(`^sj[0-9a-f]{10}n[0-9]$`)
)

// JailNIC is one NIC of a jail as StartJail needs it.
type JailNIC struct {
	Slot   int
	Bridge string
	// MAC is the jail end's address: locally administered, unicast.
	MAC string
	// DHCP is true when the jail's rc.conf asks dhclient for an address;
	// such a jail gets JailDHCPDevfsRuleset.
	DHCP bool
}

// SkepInterface is a host interface whose description says Skep made it.
type SkepInterface struct {
	Name string
	// Kind is "epair" (a jail's host end) or "tap" (a VM's).
	Kind    string
	OwnerID string
	Slot    int
}

// InterfaceDescription is the description of a VM's tap or a jail's epair
// host end: skep:<owner-id>:<slot>.
func InterfaceDescription(ownerID string, slot int) string { return TapDescription(ownerID, slot) }

// parseDescription reads skep:<id>:<slot>.
func parseDescription(desc string) (id string, slot int, ok bool) {
	rest, found := strings.CutPrefix(desc, "skep:")
	if !found {
		return "", 0, false
	}
	id, s, found := strings.Cut(rest, ":")
	if !found || !ValidID(id) {
		return "", 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > MaxSlot || strconv.Itoa(n) != s {
		return "", 0, false
	}
	return id, n, true
}

// MaxJailNICSlot bounds a jail NIC's slot: the name has one digit for it.
const MaxJailNICSlot = models.MaxContainerNICs - 1

// JailInterfaceName is the stable name of a jail NIC's jail end, the same
// on every start, so the jail's rc.conf can refer to it.
func JailInterfaceName(containerID string, slot int) (string, error) {
	if !ValidID(containerID) {
		return "", invalid("invalid container ID")
	}
	if slot < 0 || slot > MaxJailNICSlot {
		return "", invalid("invalid jail NIC slot")
	}
	hex := strings.ToLower(strings.ReplaceAll(containerID, "-", ""))
	return jailIfacePrefix + hex[:10] + "n" + strconv.Itoa(slot), nil
}

// JailInterfacePrefix is what JailInterfaceName puts before "n<slot>": two
// containers whose IDs share it would share interface names.
func JailInterfacePrefix(containerID string) string {
	hex := strings.ToLower(strings.ReplaceAll(containerID, "-", ""))
	if len(hex) < 10 {
		return ""
	}
	return hex[:10]
}

// checkNICs validates the NICs of a spec.
func checkNICs(nics []JailNIC) error {
	if len(nics) > models.MaxContainerNICs {
		return invalid("a jail has at most %d NICs", models.MaxContainerNICs)
	}
	seen := map[int]bool{}
	for _, n := range nics {
		if n.Slot < 0 || n.Slot > MaxJailNICSlot || seen[n.Slot] {
			return invalid("invalid or repeated jail NIC slot %d", n.Slot)
		}
		seen[n.Slot] = true
		mac, err := net.ParseMAC(n.MAC)
		if err != nil || len(mac) != 6 || mac[0]&1 != 0 || mac.String() != n.MAC {
			return invalid("invalid MAC address for jail NIC slot %d", n.Slot)
		}
		if !bridgeNameRE.MatchString(n.Bridge) {
			return invalid("invalid bridge name")
		}
	}
	return nil
}

var bridgeNameRE = regexp.MustCompile(`^[a-z][a-z0-9]{0,14}$`)

// devfsRuleset is the ruleset a jail's devfs gets: Skep's DHCP ruleset if
// any NIC uses DHCP, otherwise devfsrules_jail_vnet.
func (s JailSpec) devfsRuleset() int {
	for _, n := range s.NICs {
		if n.DHCP {
			return JailDHCPDevfsRuleset
		}
	}
	return JailDevfsRuleset
}

// EpairAvailable implements Jails: if_epair must be loaded (or compiled
// in). "ifconfig epair create" would load it, and Skep loads no modules.
func (h *host) EpairAvailable(ctx context.Context) error {
	if _, err := h.run.Run(ctx, kldstatBinary, "-q", "-m", "if_epair"); err != nil {
		return &models.Error{Code: models.CodeEpairUnavailable,
			Message: "the if_epair module is not loaded, so a jail cannot have a NIC; the operator loads it (kld_list=\"if_epair\" in rc.conf); Skep loads no modules"}
	}
	return nil
}

// ensureDHCPRuleset adds Skep's DHCP devfs ruleset when it is absent and
// checks it. A ruleset of that number with other rules is refused and left
// alone.
func (h *host) ensureDHCPRuleset(ctx context.Context) error {
	n := strconv.Itoa(JailDHCPDevfsRuleset)
	rules, err := h.devfsRules(ctx)
	if err != nil {
		return err
	}
	if len(rules) == 0 {
		h.log.Info("hostops: adding Skep's devfs ruleset for DHCP jails", "event", "devfs_ruleset_added",
			"ruleset", JailDHCPDevfsRuleset, "rules", strings.Join(jailDHCPRules, "; "))
		for _, rule := range jailDHCPRules {
			args := append([]string{"rule", "-s", n, "add"}, strings.Fields(rule)...)
			if _, err := h.run.Run(ctx, devfsBinary, args...); err != nil {
				return fmt.Errorf("hostops: add devfs ruleset %s: %w", n, err)
			}
		}
		if rules, err = h.devfsRules(ctx); err != nil {
			return err
		}
	}
	if strings.Join(rules, "\n") != strings.Join(jailDHCPRules, "\n") {
		return &models.Error{Code: models.CodeDevfsRulesetConflict,
			Message: fmt.Sprintf("devfs ruleset %d holds rules Skep did not put there (%s); Skep uses that number for jails with DHCP and changes nothing while it is taken",
				JailDHCPDevfsRuleset, printable(strings.Join(rules, "; "))),
			Details: map[string]any{"ruleset": JailDHCPDevfsRuleset}}
	}
	return nil
}

func (h *host) devfsRules(ctx context.Context) ([]string, error) {
	out, err := h.run.Run(ctx, devfsBinary, "rule", "-s", strconv.Itoa(JailDHCPDevfsRuleset), "show")
	if err != nil {
		return nil, fmt.Errorf("hostops: read devfs ruleset %d: %w", JailDHCPDevfsRuleset, err)
	}
	var rules []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			rules = append(rules, l)
		}
	}
	return rules, nil
}

// epairs lists the members of the interface group "epair".
func (h *host) epairs(ctx context.Context) ([]string, error) {
	out, err := h.run.Run(ctx, ifconfigBinary, "-g", "epair")
	if err != nil {
		return nil, fmt.Errorf("hostops: list epairs: %w", err)
	}
	return strings.Fields(string(out)), nil
}

// interfaceExists reports whether a host interface of that name exists.
func (h *host) interfaceExists(ctx context.Context, name string) (bool, error) {
	_, exists, err := h.tapDescription(ctx, name)
	return exists, err
}

// createJailEpair creates the epair of one NIC and returns its host end.
// The jail end is renamed, given its MAC, and left for vnet.interface;
// the host end joins the bridge.
func (h *host) createJailEpair(ctx context.Context, containerID string, nic JailNIC) (string, error) {
	jif, err := JailInterfaceName(containerID, nic.Slot)
	if err != nil {
		return "", err
	}
	if exists, err := h.interfaceExists(ctx, jif); err != nil {
		return "", err
	} else if exists {
		return "", fmt.Errorf("hostops: an interface named %s already exists on the host; Skep does not touch it", jif)
	}
	desc := InterfaceDescription(containerID, nic.Slot)
	host, err := h.epairCreate(ctx, desc)
	if err != nil {
		return "", err
	}
	peer := strings.TrimSuffix(host, "a") + "b"
	steps := [][]string{
		{peer, "name", jif},
		{jif, "ether", nic.MAC},
		{nic.Bridge, "addm", host},
	}
	for _, args := range steps {
		if _, err := h.run.Run(ctx, ifconfigBinary, args...); err != nil {
			return host, fmt.Errorf("hostops: set up the epair %s for slot %d: %w", host, nic.Slot, err)
		}
	}
	return host, nil
}

// epairCreate runs the one ifconfig call that creates, describes, and
// disables IPv6 on a host end (amendment h). If the call fails, an epair
// it created anyway is destroyed when it carries the description, and
// reported when it carries none (Skep never destroys an undescribed
// interface).
func (h *host) epairCreate(ctx context.Context, desc string) (string, error) {
	h.epairMu.Lock()
	defer h.epairMu.Unlock()
	before, err := h.epairs(ctx)
	if err != nil {
		return "", err
	}
	out, err := h.run.Run(ctx, ifconfigBinary, "epair", "create", "inet6", "description", desc, "ifdisabled", "-auto_linklocal", "up")
	if err == nil {
		name := strings.TrimSpace(string(out))
		if !epairHostRE.MatchString(name) {
			return "", fmt.Errorf("hostops: ifconfig created an interface with an unexpected name %q", printable(name))
		}
		return name, nil
	}
	after, lerr := h.epairs(ctx)
	if lerr != nil {
		return "", fmt.Errorf("hostops: create an epair: %w (and %v)", err, lerr)
	}
	old := map[string]bool{}
	for _, n := range before {
		old[n] = true
	}
	for _, n := range after {
		if old[n] || !epairHostRE.MatchString(n) {
			continue
		}
		d, exists, derr := h.tapDescription(ctx, n)
		switch {
		case derr != nil || !exists:
		case d == desc:
			if _, derr := h.run.Run(ctx, ifconfigBinary, n, "destroy"); derr != nil {
				h.log.Error("hostops: could not destroy an epair a failed create left", "epair", n, "error", derr)
			}
		case d == "":
			h.log.Error("hostops: a failed epair create may have left an undescribed epair; Skep does not destroy it",
				"event", "undescribed_epair", "epair", n)
		}
	}
	return "", fmt.Errorf("hostops: create an epair: %w", err)
}

// DestroyJailInterfaces implements Jails: it destroys every epair host end
// described skep:<container-id>:<slot>, which destroys the jail end too,
// and returns their names.
func (h *host) DestroyJailInterfaces(ctx context.Context, containerID string) ([]string, error) {
	if !ValidID(containerID) {
		return nil, invalid("invalid container ID")
	}
	names, err := h.epairs(ctx)
	if err != nil {
		return nil, err
	}
	var destroyed []string
	for _, n := range names {
		if !epairHostRE.MatchString(n) {
			continue
		}
		desc, exists, err := h.tapDescription(ctx, n)
		if err != nil {
			return destroyed, fmt.Errorf("hostops: read %s: %w", n, err)
		}
		if id, _, ok := parseDescription(desc); !exists || !ok || id != containerID {
			continue
		}
		if _, err := h.run.Run(ctx, ifconfigBinary, n, "destroy"); err != nil && !stderrContains(err, "does not exist") {
			return destroyed, fmt.Errorf("hostops: destroy %s: %w", n, err)
		}
		destroyed = append(destroyed, n)
	}
	return destroyed, nil
}

// SkepInterfaces implements Jails: every epair host end and tap whose
// description is skep:<id>:<slot>.
func (h *host) SkepInterfaces(ctx context.Context) ([]SkepInterface, error) {
	var out []SkepInterface
	for _, g := range []struct {
		group string
		re    *regexp.Regexp
	}{{"epair", epairHostRE}, {"tap", tapNameRE}} {
		raw, err := h.run.Run(ctx, ifconfigBinary, "-g", g.group)
		if err != nil {
			return nil, fmt.Errorf("hostops: list the %s interfaces: %w", g.group, err)
		}
		for _, n := range strings.Fields(string(raw)) {
			if !g.re.MatchString(n) {
				continue
			}
			desc, exists, err := h.tapDescription(ctx, n)
			if err != nil {
				return nil, fmt.Errorf("hostops: read %s: %w", n, err)
			}
			if id, slot, ok := parseDescription(desc); exists && ok {
				out = append(out, SkepInterface{Name: n, Kind: g.group, OwnerID: id, Slot: slot})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
