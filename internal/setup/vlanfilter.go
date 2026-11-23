package setup

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
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
// epair* or tap* counts, to be safe.
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
// real uplinks (the rc.conf members, or the live members). When vlanfilter
// is already in rc.conf it prints only the live steps. Any name, the
// bridge's own included, that does not match safeNameRE, or a word of the line that does not match
// safeWordRE, means no steps at all.
//
// A bridge with a guest port as a live member gets no steps at all: setup
// never sets a PVID on a guest port (Andy's decision, 2026-11-23). The
// agent sets each one when its guest starts, so the item says to stop the
// guests on the bridge and run plan again.
func vlanfilterSteps(br, line string, inRC, rcFilter bool, rcMembers, now []string, exists, isLive bool, rc RCConf) Item {
	if !safeNameRE.MatchString(br) {
		return Item{Kind: Manual, Area: "network", What: strconv.Quote(br) + " vlanfilter",
			Detail: fmt.Sprintf("not set; setup prints no steps: the bridge name %q is not a plain interface name (letters, digits, dot, underscore), and setup will not put it into a command. Set the PVIDs, defuntagged and vlanfilter by hand. Without it the agent reports bridge_vlan_unfiltered", br)}
	}
	it := Item{Kind: Manual, Area: "network", What: br + " vlanfilter"}

	if isLive {
		var guests []string
		for _, x := range now {
			if guestPort(x) {
				guests = append(guests, strconv.Quote(x))
			}
		}
		if len(guests) > 0 {
			it.Detail = fmt.Sprintf("not set; setup prints no steps while Skep guest ports are members (%s): stop the Skep guests on this bridge first, then run plan again. The agent sets each guest port when the guest starts. Without it the agent reports bridge_vlan_unfiltered", strings.Join(guests, ", "))
			return it
		}
	}

	var liveSteps []string
	if exists {
		for _, x := range now {
			liveSteps = append(liveSteps, fmt.Sprintf("ifconfig %s ifuntagged %s 1", br, x))
		}
		liveSteps = append(liveSteps, "ifconfig "+br+" defuntagged 1", "ifconfig "+br+" vlanfilter")
	}

	if rcFilter {
		// (a) Only the live state is off: the line already has it.
		it.Detail = "in ifconfig_" + br + " in rc.conf but off now: it takes effect at the next boot, or run these steps from the console to turn it on now (each member's PVID first, then defuntagged, then vlanfilter, then each member up). Until then the agent reports bridge_vlan_unfiltered"
		if bad := unsafeNames(append(slices.Clone(now), rcMembers...)); bad != "" {
			return noSteps(it, bad)
		}
		it.Steps = append(liveSteps, upLive(br, now, exists)...)
		persist, note := uplinksUp(rcMembers, rc)
		it.Steps = append(it.Steps, persist...)
		if len(persist) == 0 && note == "" {
			it.Detail += ". rc.conf needs no change"
		}
		it.Detail += note
		return it
	}

	// The members the persisted line names: real uplinks only.
	var uplinks []string
	if inRC {
		uplinks = rcMembers
	} else if isLive {
		uplinks = now
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

	it.Steps = append(liveSteps, upLive(br, now, exists)...)
	if !inRC {
		// (b) A bridge rc.conf does not create is gone after a reboot,
		// whatever its ifconfig_ line says.
		if cloned, _ := rc.Get("cloned_interfaces"); !slices.Contains(strings.Fields(cloned), br) {
			it.Steps = append(it.Steps, fmt.Sprintf(`sysrc cloned_interfaces+="%s"`, br))
		}
	}
	it.Steps = append(it.Steps, fmt.Sprintf(`sysrc ifconfig_%s="%s"`, br, strings.Join(words, " ")))
	persist, note := uplinksUp(uplinks, rc)
	it.Steps = append(it.Steps, persist...)

	switch {
	case exists:
		it.Detail = "not set; changing an existing bridge can cut off its members, so setup prints the steps (each member's PVID first, then defuntagged, then vlanfilter, then each member up); run them from the console. Without it the agent reports bridge_vlan_unfiltered"
	default:
		it.Detail = "not set in ifconfig_" + br + " in rc.conf; the bridge does not exist now, so only the rc.conf line is printed (it takes effect when the bridge is created at the next boot). Without it the agent reports bridge_vlan_unfiltered"
	}
	it.Detail += note
	return it
}

// upLive is the live step for each member: adding a member to a bridge
// does not bring it up, and a bridge whose uplink is down passes nothing.
// It comes after vlanfilter, so no frame crosses the bridge unfiltered.
func upLive(br string, members []string, exists bool) []string {
	if !exists {
		return nil
	}
	var out []string
	for _, m := range members {
		out = append(out, "ifconfig "+m+" up")
	}
	return out
}

// uplinksUp is what keeps each uplink up across reboots: a sysrc step for
// one with no ifconfig_ line. An existing line is never replaced; if it
// neither says up nor configures an address by DHCP, note says to add up.
func uplinksUp(uplinks []string, rc RCConf) (steps []string, note string) {
	var edit []string
	for _, u := range uplinks {
		line, ok := rc.Get("ifconfig_" + u)
		if !ok {
			steps = append(steps, fmt.Sprintf(`sysrc ifconfig_%s="up"`, u))
			continue
		}
		words := strings.Fields(strings.ToLower(line))
		if !slices.Contains(words, "up") && !slices.Contains(words, "dhcp") && !slices.Contains(words, "syncdhcp") {
			edit = append(edit, u)
		}
	}
	if len(edit) > 0 {
		note = fmt.Sprintf(". Add the word up to the ifconfig_ line of %v in rc.conf by hand: without it the uplink is down after a reboot and the bridge passes nothing", edit)
	}
	return steps, note
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
