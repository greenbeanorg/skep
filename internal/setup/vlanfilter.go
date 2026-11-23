package setup

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var (
	// safeNameRE is what an interface name must match before setup puts it
	// into a printed command: a name from ifconfig or rc.conf that does not
	// match (quotes, $, spaces, ...) gets no step, and the item says why.
	safeNameRE = regexp.MustCompile(`^[A-Za-z0-9._]+$`)
	// safeWordRE is what each word of an existing ifconfig_bridgeN line must
	// match before setup reprints the line inside sysrc "...". Addresses
	// (192.0.2.1/24, fe80::1) and negated flags (-learn) pass.
	safeWordRE = regexp.MustCompile(`^[A-Za-z0-9._:/-]+$`)
)

// guestPort reports whether a live bridge member is a port the agent
// creates for a guest and destroys when it stops: a jail's epair host end
// (epairNa) or a VM's tap (tapN), hostops' epairHostRE and tapNameRE. Any
// epair* or tap* counts, to be safe: such a port never belongs in the
// rc.conf line, which would name an interface that does not exist at
// boot.
func guestPort(name string) bool {
	return strings.HasPrefix(name, "epair") || strings.HasPrefix(name, "tap")
}

// vlanfilterSteps is the item for an allow-listed bridge whose vlanfilter
// is off now (a live bridge) or not in rc.conf (one not created yet).
// line is the bridge's ifconfig_ line, inRC whether rc.conf has it,
// rcFilter whether that line has vlanfilter, rcMembers its addm members.
// now are the members it has now (ifconfig's, or rc.conf's when only
// "ifconfig -l" saw it), exists whether it exists now, and isLive whether
// now came from ifconfig.
//
// It prints, in order: the live steps (each live member's PVID, then
// defuntagged, then vlanfilter) for a bridge that exists now; for a
// run-time bridge, the cloned_interfaces step unless rc.conf already has
// it; and the sysrc line that keeps it across reboots, which names only
// real uplinks (the rc.conf members, or the live members that are not
// guest ports). When vlanfilter is already in rc.conf it prints only the
// live steps. Any name that does not match safeNameRE, or a word of the
// line that does not match safeWordRE, means no steps at all.
func vlanfilterSteps(br, line string, inRC, rcFilter bool, rcMembers, now []string, exists, isLive bool, rc RCConf) Item {
	it := Item{Kind: Manual, Area: "network", What: br + " vlanfilter"}

	var liveSteps []string
	if exists {
		for _, x := range now {
			liveSteps = append(liveSteps, fmt.Sprintf("ifconfig %s ifuntagged %s 1", br, x))
		}
		liveSteps = append(liveSteps, "ifconfig "+br+" defuntagged 1", "ifconfig "+br+" vlanfilter")
	}

	if rcFilter {
		// (a) Only the live state is off: the line already has it.
		it.Detail = "in ifconfig_" + br + " in rc.conf but off now: it takes effect at the next boot, or run these steps from the console to turn it on now (each member's PVID first, then defuntagged, then vlanfilter). rc.conf needs no change. Until then the agent reports bridge_vlan_unfiltered"
		if bad := unsafeNames(now); bad != "" {
			return noSteps(it, bad)
		}
		it.Steps = liveSteps
		return it
	}

	// The members the persisted line names: real uplinks only.
	var uplinks, left []string
	if inRC {
		uplinks = rcMembers
	} else if isLive {
		for _, x := range now {
			if guestPort(x) {
				left = append(left, x)
			} else {
				uplinks = append(uplinks, x)
			}
		}
	}
	if bad := unsafeNames(append(slices.Clone(now), uplinks...)); bad != "" {
		return noSteps(it, bad)
	}
	var words []string
	if inRC {
		for _, w := range strings.Fields(line) {
			if !safeWordRE.MatchString(w) {
				it.Detail = fmt.Sprintf("not set; setup prints no steps: ifconfig_%s in rc.conf has the word %q, which setup will not reprint inside a command (letters, digits, and . _ : / - only). Edit the line by hand: add ifuntagged MEMBER 1 for each uplink, then defuntagged 1 vlanfilter. Without it the agent reports bridge_vlan_unfiltered", br, w)
				return it
			}
			if w != "up" {
				words = append(words, w)
			}
		}
	} else {
		for _, x := range uplinks {
			words = append(words, "addm", x)
		}
	}
	for _, x := range uplinks {
		if !hasPair(words, "ifuntagged", x) {
			words = append(words, "ifuntagged", x, "1")
		}
	}
	if !slices.Contains(words, "defuntagged") {
		words = append(words, "defuntagged", "1")
	}
	words = append(words, "vlanfilter", "up")

	it.Steps = liveSteps
	if !inRC {
		// (b) A bridge rc.conf does not create is gone after a reboot,
		// whatever its ifconfig_ line says.
		if cloned, _ := rc.Get("cloned_interfaces"); !slices.Contains(strings.Fields(cloned), br) {
			it.Steps = append(it.Steps, fmt.Sprintf(`sysrc cloned_interfaces+="%s"`, br))
		}
	}
	it.Steps = append(it.Steps, fmt.Sprintf(`sysrc ifconfig_%s="%s"`, br, strings.Join(words, " ")))

	switch {
	case exists:
		it.Detail = "not set; changing an existing bridge can cut off its members, so setup prints the steps (each member's PVID first, then defuntagged, then vlanfilter); run them from the console. Without it the agent reports bridge_vlan_unfiltered"
	default:
		it.Detail = "not set in ifconfig_" + br + " in rc.conf; the bridge does not exist now, so only the rc.conf line is printed (it takes effect when the bridge is created at the next boot). Without it the agent reports bridge_vlan_unfiltered"
	}
	if len(left) > 0 {
		it.Detail += fmt.Sprintf(". The rc.conf line leaves out %v: guest ports the agent creates when a guest starts", left)
	}
	return it
}

// unsafeNames returns the first name that does not match safeNameRE.
func unsafeNames(names []string) string {
	for _, n := range names {
		if !safeNameRE.MatchString(n) {
			return n
		}
	}
	return ""
}

// noSteps is it with no steps, saying which name stopped them.
func noSteps(it Item, bad string) Item {
	it.Steps = nil
	it.Detail = fmt.Sprintf("not set; setup prints no steps: the member name %q is not a plain interface name (letters, digits, dot, underscore), and setup will not put it into a command. Set the PVIDs, defuntagged and vlanfilter by hand. Without it the agent reports bridge_vlan_unfiltered", bad)
	return it
}

// hasPair reports whether words has key followed by val.
func hasPair(words []string, key, val string) bool {
	for i := 0; i+1 < len(words); i++ {
		if words[i] == key && words[i+1] == val {
			return true
		}
	}
	return false
}
