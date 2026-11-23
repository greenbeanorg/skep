package setup

import (
	"strings"
	"testing"
)

// A bridge name that is not a plain interface name reaches no printed
// command, in any of the shapes vlanfilterSteps handles.
func TestVlanfilterStepsRefusesUnsafeBridgeName(t *testing.T) {
	names := []string{`bridge0;id`, `bridge0 x`, `$(id)`, "`id`", `br"0`, "br\n0", ""}
	shapes := []struct {
		name                         string
		line                         string
		inRC, rcFilter, exists, live bool
		rcMembers, now               []string
	}{
		{name: "live, not in rc.conf", exists: true, live: true, now: []string{"vtnet1"}},
		{name: "live, line without vlanfilter", line: "addm vtnet1 up", inRC: true, exists: true, live: true,
			rcMembers: []string{"vtnet1"}, now: []string{"vtnet1"}},
		{name: "live off, line has vlanfilter", line: "addm vtnet1 vlanfilter up", inRC: true, rcFilter: true, exists: true, live: true,
			rcMembers: []string{"vtnet1"}, now: []string{"vtnet1"}},
		{name: "not created yet", line: "addm vtnet1 up", inRC: true, rcMembers: []string{"vtnet1"}},
		{name: "guest port member", exists: true, live: true, now: []string{"epair0a"}},
	}
	for _, br := range names {
		for _, s := range shapes {
			it := vlanfilterSteps(br, s.line, s.inRC, s.rcFilter, s.rcMembers, s.now, s.exists, s.live, RCConf{})
			if len(it.Steps) != 0 {
				t.Errorf("%q, %s: steps %q, want none", br, s.name, it.Steps)
			}
			if it.Kind != Manual || !strings.Contains(it.Detail, "the bridge name") {
				t.Errorf("%q, %s: kind %v, detail %q: must say the bridge name stopped the steps", br, s.name, it.Kind, it.Detail)
			}
			if strings.ContainsAny(it.What, "\n") || strings.ContainsAny(it.Detail, "\n") {
				t.Errorf("%q, %s: a raw newline reached the report: %q / %q", br, s.name, it.What, it.Detail)
			}
		}
	}
}

// A plain name still gets its steps.
func TestVlanfilterStepsKeepsPlainBridgeName(t *testing.T) {
	it := vlanfilterSteps("bridge0", "", false, false, nil, []string{"vtnet1"}, true, true, RCConf{})
	if len(it.Steps) == 0 || !strings.Contains(it.Steps[0], "ifconfig bridge0 ifuntagged vtnet1 1") {
		t.Fatalf("steps = %q", it.Steps)
	}
}
