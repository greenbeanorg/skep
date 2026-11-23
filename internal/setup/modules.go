package setup

import (
	"path/filepath"
	"slices"
	"strings"
)

// GuestModules are the kernel modules Skep's guests need, in the order
// setup reports them: vmm and nmdm, which setup configures in loader.conf,
// then KLDModules, which it configures in rc.conf's kld_list.
var GuestModules = []string{"vmm", "nmdm", "if_epair", "netgraph", "ng_socket", "ng_ether", "ng_bpf"}

// loaderModules are the guest modules loaded by a NAME_load line in
// loader.conf.
var loaderModules = []string{"vmm", "nmdm"}

// moduleUse says what each guest module is for, in the report of modules
// that are configured but not loaded.
var moduleUse = map[string]string{
	"vmm":       "bhyve VMs",
	"nmdm":      "VM serial consoles",
	"if_epair":  "jail network interfaces",
	"netgraph":  "guest port anti-spoofing filters (ADR 0011)",
	"ng_socket": "guest port anti-spoofing filters (ADR 0011)",
	"ng_ether":  "guest port anti-spoofing filters (ADR 0011)",
	"ng_bpf":    "guest port anti-spoofing filters (ADR 0011)",
}

// ConfiguredModules returns the guest modules the host's files load at the
// next boot: vmm and nmdm when loader.conf (or loader.conf.local or
// loader.conf.d) sets NAME_load="YES", and each of KLDModules that rc.conf
// (or an override) lists in kld_list, by name, as NAME.ko or by path.
func ConfiguredModules(rc, loader RCConf) []string {
	var out []string
	kld := map[string]bool{}
	if v, ok := rc.Get("kld_list"); ok {
		for _, e := range strings.Fields(v) {
			kld[strings.TrimSuffix(filepath.Base(e), ".ko")] = true
		}
	}
	for _, m := range GuestModules {
		if slices.Contains(loaderModules, m) {
			if v, ok := loader.Get(m + "_load"); ok && strings.EqualFold(v, "YES") {
				out = append(out, m)
			}
		} else if kld[m] {
			out = append(out, m)
		}
	}
	return out
}

// HostModules reads the boot configuration under root and returns
// ConfiguredModules for it: after apply, what the next boot will load.
func HostModules(root string) ([]string, error) {
	rc, err := ReadRCConf(root)
	if err != nil {
		return nil, err
	}
	loader, err := readLoader(root)
	if err != nil {
		return nil, err
	}
	return ConfiguredModules(rc, loader), nil
}

// NotLoaded returns the configured modules the kernel does not have now.
// known is false when the facts do not say (kldstat could not be read).
func NotLoaded(configured []string, f Facts) (missing []string, known bool) {
	if f.Modules == nil {
		return nil, false
	}
	for _, m := range configured {
		if !f.Modules[m] {
			missing = append(missing, m)
		}
	}
	return missing, true
}

// KldloadCommand is the command that loads the given modules now.
func KldloadCommand(missing []string) string {
	return "kldload -n " + strings.Join(missing, " ")
}

// ModulesItem reports the configured guest modules against what the
// kernel has now. Setup never loads a module (it would change the running
// kernel, which apply never does); a module configured but not loaded is
// a warning with the two ways to load it: a reboot, or kldload. It does
// not change any exit code. ok is false when there is nothing to report
// (no guest module configured).
func ModulesItem(configured []string, f Facts) (Item, bool) {
	if len(configured) == 0 {
		return Item{}, false
	}
	missing, known := NotLoaded(configured, f)
	if !known {
		return Item{Kind: Warn, Area: "kernel modules", What: "whether they are loaded is unknown",
			Detail: "kldstat could not be read; configured: " + strings.Join(configured, " ")}, true
	}
	if len(missing) == 0 {
		return Item{Kind: OK, Area: "kernel modules", What: "configured modules are loaded",
			Detail: strings.Join(configured, " ")}, true
	}
	var uses []string
	for _, m := range missing {
		uses = append(uses, m+": "+moduleUse[m])
	}
	detail := "configured to load at boot, not loaded now; until they are, these do not work:\n" +
		strings.Join(uses, "\n") + "\n" +
		"and the agent's health can read degraded (skep-setup upgrade then exits 3).\n" +
		"Setup never loads modules. Either reboot, or load them now:"
	return Item{Kind: Warn, Area: "kernel modules", What: "configured but not loaded: " + strings.Join(missing, " "),
		Detail: detail, Steps: []string{"shutdown -r now", KldloadCommand(missing)}}, true
}

// plannedModules adds to configured the guest modules this plan's Add
// items configure, so plan and check report a module that apply is about
// to configure and that will still not be loaded afterwards.
func plannedModules(configured []string, items []Item) []string {
	have := map[string]bool{}
	for _, m := range configured {
		have[m] = true
	}
	for _, it := range items {
		if it.Kind != Add || it.Action == nil || it.Action.Type != ActSysrc {
			continue
		}
		a := it.Action
		switch {
		case a.File == loaderConfPath && strings.HasSuffix(a.Name, "_load") && strings.EqualFold(a.Value, "YES"):
			have[strings.TrimSuffix(a.Name, "_load")] = true
		case a.File == rcConfPath && a.Name == "kld_list":
			for _, e := range strings.Fields(a.Value) {
				have[strings.TrimSuffix(filepath.Base(e), ".ko")] = true
			}
		}
	}
	var out []string
	for _, m := range GuestModules {
		if have[m] {
			out = append(out, m)
		}
	}
	return out
}

func (b *builder) liveModules() {
	configured := plannedModules(ConfiguredModules(b.rc, b.loader), b.plan.Items)
	if it, ok := ModulesItem(configured, b.f); ok {
		b.item(it)
	}
}
