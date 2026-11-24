package setup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	vidTunable  = "hw.vmm.vmx.cap.virtual_interrupt_delivery"
	completeFix = "A reboot is the complete fix. kldload loads modules only: loader tunables added since boot need the reboot"
	vmmHeld     = "Loader tunables for vmm set since the last boot are not applied by kldload; reboot"
)

func loaderWith(vals map[string]string) RCConf {
	return RCConf{Main: ShVars{Path: "/boot/loader.conf", Vals: vals}}
}

func kenvOf(vals map[string]string) func(string) (string, bool) {
	return func(n string) (string, bool) {
		v, ok := vals[n]
		return v, ok
	}
}

func TestModulesItemHoldsBackVMM(t *testing.T) {
	missing := []string{"vmm", "nmdm"}
	f := func(kenv func(string) (string, bool)) Facts { return Facts{Modules: loaded(), Kenv: kenv} }
	withVID := loaderWith(map[string]string{vidTunable: "0"})
	cases := []struct {
		name   string
		loader RCConf
		kenv   func(string) (string, bool)
		steps  []string
		held   bool
	}{
		{"same value", withVID, kenvOf(map[string]string{vidTunable: "0"}), []string{"shutdown -r now", "kldload -n vmm nmdm"}, false},
		{"no hw.vmm line", loaderWith(map[string]string{}), nil, []string{"shutdown -r now", "kldload -n vmm nmdm"}, false},
		{"differs", withVID, kenvOf(map[string]string{vidTunable: "1"}), []string{"shutdown -r now", "kldload -n nmdm"}, true},
		{"absent from kenv", withVID, kenvOf(nil), []string{"shutdown -r now", "kldload -n nmdm"}, true},
		{"kenv cannot be read", withVID, nil, []string{"shutdown -r now", "kldload -n nmdm"}, true},
		{"override differs", RCConf{Main: ShVars{Vals: map[string]string{vidTunable: "0"}},
			Overrides: []ShVars{{Path: "/boot/loader.conf.local", Vals: map[string]string{vidTunable: "1"}}}},
			kenvOf(map[string]string{vidTunable: "0"}), []string{"shutdown -r now", "kldload -n nmdm"}, true},
		{"line setup cannot parse", RCConf{Main: ShVars{Path: "/boot/loader.conf", Vals: map[string]string{},
			Complex: []string{vidTunable + " = 0"}}}, kenvOf(nil), []string{"shutdown -r now", "kldload -n nmdm"}, true},
	}
	for _, c := range cases {
		it, ok := ModulesItem(missing, f(c.kenv), c.loader, false)
		if !ok || it.Kind != Warn {
			t.Fatalf("%s: %+v", c.name, it)
		}
		if !slices.Equal(it.Steps, c.steps) {
			t.Errorf("%s: steps %q, want %q", c.name, it.Steps, c.steps)
		}
		if !strings.Contains(it.Detail, completeFix) {
			t.Errorf("%s: no complete-fix sentence:\n%s", c.name, it.Detail)
		}
		if got := strings.Contains(it.Detail, vmmHeld); got != c.held {
			t.Errorf("%s: held sentence = %v, want %v:\n%s", c.name, got, c.held, it.Detail)
		}
	}
}

// With vmm the only missing module and held back, only the reboot is left.
func TestModulesItemOnlyVMMHeldHasNoKldload(t *testing.T) {
	it, _ := ModulesItem([]string{"vmm"}, Facts{Modules: loaded(), Kenv: kenvOf(nil)},
		loaderWith(map[string]string{vidTunable: "0"}), false)
	if !slices.Equal(it.Steps, []string{"shutdown -r now"}) {
		t.Fatalf("steps %q", it.Steps)
	}
}

// A tunable does not hold back a module other than vmm.
func TestModulesItemTunableIgnoredWhenVMMLoaded(t *testing.T) {
	it, _ := ModulesItem([]string{"vmm", "nmdm"}, Facts{Modules: loaded("vmm"), Kenv: kenvOf(nil)},
		loaderWith(map[string]string{vidTunable: "0"}), false)
	if !slices.Equal(it.Steps, []string{"shutdown -r now", "kldload -n nmdm"}) || strings.Contains(it.Detail, vmmHeld) {
		t.Fatalf("%+v", it)
	}
}

func TestVMMTunablesNeverPassAnUnsafeNameToKenv(t *testing.T) {
	var asked []string
	kenv := func(n string) (string, bool) { asked = append(asked, n); return "0", true }
	got := vmmTunablesNotApplied(loaderWith(map[string]string{"hw.vmm.": "0", vidTunable: "0"}), kenv)
	if !slices.Equal(got, []string{"hw.vmm."}) || !slices.Equal(asked, []string{vidTunable}) {
		t.Fatalf("got %q, asked %q", got, asked)
	}
}

// The text of a line setup cannot parse is never printed.
func TestVMMTunablesDoNotPrintAnUnparsedLine(t *testing.T) {
	got := vmmTunablesNotApplied(RCConf{Main: ShVars{Path: "/boot/loader.conf", Vals: map[string]string{},
		Complex: []string{`hw.vmm.x = "$(id)"`}}}, kenvOf(nil))
	if !slices.Equal(got, []string{"a line setup cannot read in loader.conf"}) {
		t.Fatalf("%q", got)
	}
}

func TestModulesItemNamesRacct(t *testing.T) {
	racct := loaderWith(map[string]string{"kern.racct.enable": "1"})
	const name = "need the reboot (kern.racct.enable)"
	for _, c := range []struct {
		name    string
		loader  RCConf
		kenv    map[string]string
		planned bool
		want    bool
	}{
		{"added since boot", racct, nil, false, true},
		{"set at boot", racct, map[string]string{"kern.racct.enable": "1"}, false, false},
		{"planned", loaderWith(map[string]string{}), nil, true, true},
		{"not configured", loaderWith(map[string]string{}), nil, false, false},
	} {
		it, _ := ModulesItem([]string{"nmdm"}, Facts{Modules: loaded(), Kenv: kenvOf(c.kenv)}, c.loader, c.planned)
		if got := strings.Contains(it.Detail, name); got != c.want {
			t.Errorf("%s: named = %v, want %v:\n%s", c.name, got, c.want, it.Detail)
		}
	}
}

func TestModulesHintFollowsTheSameRule(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "boot"), 0o755); err != nil {
		t.Fatal(err)
	}
	conf := "vmm_load=\"YES\"\nnmdm_load=\"YES\"\n" + vidTunable + "=\"0\"\n"
	if err := os.WriteFile(filepath.Join(root, "boot/loader.conf"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	same := modulesHint(root, Facts{Modules: loaded(), Kenv: kenvOf(map[string]string{vidTunable: "0"})})
	if !strings.Contains(same, completeFix) || !strings.Contains(same, "kldload -n vmm nmdm") || strings.Contains(same, vmmHeld) {
		t.Errorf("same value: %s", same)
	}
	diff := modulesHint(root, Facts{Modules: loaded(), Kenv: kenvOf(map[string]string{vidTunable: "1"})})
	if !strings.Contains(diff, vmmHeld) || !strings.Contains(diff, "kldload -n nmdm") || strings.Contains(diff, "kldload -n vmm") {
		t.Errorf("differs: %s", diff)
	}
	if got := modulesHint(root, Facts{Modules: loaded("vmm", "nmdm")}); got != "" {
		t.Errorf("all loaded: %q", got)
	}
}
