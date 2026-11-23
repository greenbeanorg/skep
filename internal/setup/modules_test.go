package setup

import (
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestGuestModulesCoverKLDModules(t *testing.T) {
	want := append(slices.Clone(loaderModules), KLDModules...)
	if !slices.Equal(GuestModules, want) {
		t.Fatalf("GuestModules %v, want loader modules then KLDModules %v", GuestModules, want)
	}
	for _, m := range GuestModules {
		if moduleUse[m] == "" {
			t.Errorf("no use recorded for %s", m)
		}
	}
}

func shvars(vals map[string]string) ShVars { return ShVars{Vals: vals} }

func TestConfiguredModules(t *testing.T) {
	cases := []struct {
		name       string
		rc, loader RCConf
		want       []string
	}{
		{"nothing", RCConf{Main: shvars(map[string]string{})}, RCConf{Main: shvars(map[string]string{})}, nil},
		{"loader yes in any case, kld by name, .ko and path",
			RCConf{Main: shvars(map[string]string{"kld_list": "if_epair netgraph.ko /boot/kernel/ng_bpf.ko other"})},
			RCConf{Main: shvars(map[string]string{"vmm_load": "yes", "nmdm_load": "NO"})},
			[]string{"vmm", "if_epair", "netgraph", "ng_bpf"}},
		{"overrides win",
			RCConf{Main: shvars(map[string]string{"kld_list": "if_epair"}), Overrides: []ShVars{shvars(map[string]string{"kld_list": "ng_socket"})}},
			RCConf{Main: shvars(map[string]string{"vmm_load": "YES"}), Overrides: []ShVars{shvars(map[string]string{"vmm_load": "NO", "nmdm_load": "YES"})}},
			[]string{"nmdm", "ng_socket"}},
	}
	for _, c := range cases {
		if got := ConfiguredModules(c.rc, c.loader); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func loaded(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range GuestModules {
		m[n] = slices.Contains(names, n)
	}
	return m
}

func TestModulesItem(t *testing.T) {
	if _, ok := ModulesItem(nil, Facts{Modules: loaded()}, RCConf{}, false); ok {
		t.Error("an item although no module is configured")
	}
	it, ok := ModulesItem([]string{"vmm"}, Facts{}, RCConf{}, false)
	if !ok || it.Kind != Warn || !strings.Contains(it.What, "unknown") {
		t.Errorf("unknown load state: %+v", it)
	}
	it, _ = ModulesItem([]string{"vmm", "nmdm"}, Facts{Modules: loaded("vmm", "nmdm")}, RCConf{}, false)
	if it.Kind != OK {
		t.Errorf("all loaded: %+v", it)
	}
	it, _ = ModulesItem([]string{"vmm", "nmdm", "netgraph"}, Facts{Modules: loaded("nmdm")}, RCConf{}, false)
	if it.Kind != Warn || it.What != "configured but not loaded: vmm netgraph" {
		t.Fatalf("missing: %+v", it)
	}
	if want := []string{"shutdown -r now", "kldload -n vmm netgraph"}; !slices.Equal(it.Steps, want) {
		t.Errorf("steps %q, want %q", it.Steps, want)
	}
	for _, s := range []string{"vmm: bhyve VMs", "netgraph: guest port anti-spoofing", "Setup never loads modules", "reboot"} {
		if !strings.Contains(it.Detail, s) {
			t.Errorf("detail lacks %q:\n%s", s, it.Detail)
		}
	}
	if strings.Contains(it.Detail, "nmdm") {
		t.Errorf("detail names a loaded module:\n%s", it.Detail)
	}
}

// After the first apply on a stock host, vmm_load, nmdm_load and kld_list
// are planned; none of the modules is loaded, so plan and check say so.
func TestPlanReportsModulesThePlanConfigures(t *testing.T) {
	root := stockHost(t)
	f := facts()
	f.Modules = loaded()
	p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}, CreateBridge: "bridge0", Uplink: "vtnet1", Pools: []string{"tank"}}, f)
	it := want(t, p, Warn, "configured but not loaded", "")
	if it.What != "configured but not loaded: vmm nmdm if_epair netgraph ng_socket ng_ether ng_bpf" {
		t.Errorf("what %q", it.What)
	}
	// Nothing to apply for it: setup never loads a module.
	for _, a := range p.Actions() {
		if strings.Contains(a.String(), "kldload") {
			t.Errorf("an action loads a module: %s", a)
		}
	}
}

// The hearst fixture configures vmm and nmdm in loader.conf and lacks the
// kld_list modules, which the plan adds.
func TestPlanModulesHearst(t *testing.T) {
	root := fixture(t, "hearst")
	f := facts()
	f.Interfaces["vtnet0"] = []netip.Prefix{pfx("10.79.20.170/24")}
	f.Interfaces["bridge0"] = nil
	f.Pools = map[string]string{"skeptest": "ONLINE"}
	o := Options{Root: root, Mgmt: []string{"vtnet0"}, Bridges: []string{"bridge0"}, Pools: []string{"skeptest"}}

	f.Modules = loaded(GuestModules...)
	want(t, compute(t, o, f), OK, "configured modules are loaded", "vmm nmdm if_epair")

	f.Modules = loaded("vmm", "nmdm", "if_epair")
	it := want(t, compute(t, o, f), Warn, "configured but not loaded: netgraph ng_socket ng_ether ng_bpf", "")
	if it.Steps[1] != "kldload -n netgraph ng_socket ng_ether ng_bpf" {
		t.Errorf("steps %q", it.Steps)
	}

	f.Modules = nil
	want(t, compute(t, o, f), Warn, "whether they are loaded is unknown", "")
}

func TestHostModules(t *testing.T) {
	root := stockHost(t)
	got, err := HostModules(root)
	if err != nil || len(got) != 0 {
		t.Fatalf("stock host: %v, %v", got, err)
	}
	appendFile(t, filepath.Join(root, "boot/loader.conf"), "vmm_load=\"YES\"\n")
	appendFile(t, filepath.Join(root, "etc/rc.conf"), "kld_list=\"if_epair ng_bpf\"\n")
	got, err = HostModules(root)
	if err != nil || !slices.Equal(got, []string{"vmm", "if_epair", "ng_bpf"}) {
		t.Fatalf("after apply: %v, %v", got, err)
	}
}
