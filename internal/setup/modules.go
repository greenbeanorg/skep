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

// moduleAdvice is what setup may tell the operator to load now.
type moduleAdvice struct {
	// Load are the missing modules a kldload may load now.
	Load []string
	// VMMHeld names the hw.vmm.* loader lines the running kernel
	// environment does not have with the same value. vmm reads them when
	// it loads, from that environment, so it is left out of Load.
	VMMHeld []string
	// Racct is true when kern.racct.enable was added since boot, or the
	// plan is about to add it: only a reboot applies it.
	Racct bool
}

// adviseLoad decides what may be loaded live. A kldload does not read
// loader.conf: vmm would come up without any hw.vmm.* tunable set since the
// last boot, which can be unsafe (the virtual interrupt delivery tunable
// is one), so vmm is held back unless every such line is in the kernel
// environment with the same value.
func adviseLoad(missing []string, loader RCConf, kenv func(string) (string, bool), racctPlanned bool) moduleAdvice {
	a := moduleAdvice{Load: slices.Clone(missing),
		Racct: racctPlanned || tunableDiffers(loader, "kern.racct.enable", kenv)}
	if slices.Contains(missing, "vmm") {
		if a.VMMHeld = vmmTunablesNotApplied(loader, kenv); len(a.VMMHeld) > 0 {
			a.Load = slices.DeleteFunc(a.Load, func(m string) bool { return m == "vmm" })
		}
	}
	return a
}

// tunableDiffers reports whether loader sets name to a value the kernel
// environment does not hold. An unreadable environment differs.
func tunableDiffers(loader RCConf, name string, kenv func(string) (string, bool)) bool {
	want, ok := loader.Get(name)
	if !ok {
		return false
	}
	if kenv == nil {
		return true
	}
	got, ok := kenv(name)
	return !ok || got != want
}

// vmmTunablePrefix is the namespace of the tunables vmm reads when it loads.
const vmmTunablePrefix = "hw.vmm."

// vmmTunablesNotApplied lists the hw.vmm.* lines of loader.conf,
// loader.conf.local and loader.conf.d that differ from, or are absent from,
// the kernel environment. A line setup cannot parse is counted as differing
// and named by its file only: its text is never printed.
func vmmTunablesNotApplied(loader RCConf, kenv func(string) (string, bool)) []string {
	files := append([]ShVars{loader.Main}, loader.Overrides...)
	names := map[string]bool{}
	var out []string
	for _, v := range files {
		for n := range v.Vals {
			if strings.HasPrefix(n, vmmTunablePrefix) {
				names[n] = true
			}
		}
		for _, line := range v.Complex {
			if strings.Contains(line, vmmTunablePrefix) {
				out = append(out, "a line setup cannot read in "+filepath.Base(v.Path))
			}
		}
	}
	for n := range names {
		if !tunableNameRE.MatchString(n) || tunableDiffers(loader, n, kenv) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// rebootNote says, in every case, that a reboot is the complete fix and
// what kldload does not do.
func (a moduleAdvice) rebootNote() string {
	s := "A reboot is the complete fix. kldload loads modules only: loader tunables added since boot need the reboot"
	if a.Racct {
		s += " (kern.racct.enable)"
	}
	s += "."
	if len(a.VMMHeld) > 0 {
		s += " Loader tunables for vmm set since the last boot are not applied by kldload; reboot (differs from or is absent from the running kernel environment: " +
			strings.Join(a.VMMHeld, ", ") + "). vmm is left out of the kldload command."
	}
	return s
}

// ModulesItem reports the configured guest modules against what the
// kernel has now. Setup never loads a module (it would change the running
// kernel, which apply never does); a module configured but not loaded is
// a warning with the reboot, which is always the complete fix, and a
// kldload of the modules that are safe to load live (adviseLoad). It does
// not change any exit code. ok is false when there is nothing to report
// (no guest module configured). racctPlanned says the plan is about to add
// kern.racct.enable.
func ModulesItem(configured []string, f Facts, loader RCConf, racctPlanned bool) (Item, bool) {
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
	advice := adviseLoad(missing, loader, f.Kenv, racctPlanned)
	detail := "configured to load at boot, not loaded now; until they are, these do not work:\n" +
		strings.Join(uses, "\n") + "\n" +
		"and the agent's health can read degraded (skep-setup upgrade then exits 4, incomplete).\n" +
		"Setup never loads modules. " + advice.rebootNote()
	steps := []string{"shutdown -r now"}
	if len(advice.Load) > 0 {
		detail += "\nReboot, or load these now:"
		steps = append(steps, KldloadCommand(advice.Load))
	} else {
		detail += "\nReboot:"
	}
	return Item{Kind: Warn, Area: "kernel modules", What: "configured but not loaded: " + strings.Join(missing, " "),
		Detail: detail, Steps: steps}, true
}

// HostModulesItem is ModulesItem for the boot configuration under root,
// as it is now.
func HostModulesItem(root string, f Facts) (Item, bool, error) {
	rc, err := ReadRCConf(root)
	if err != nil {
		return Item{}, false, err
	}
	loader, err := readLoader(root)
	if err != nil {
		return Item{}, false, err
	}
	it, ok := ModulesItem(ConfiguredModules(rc, loader), f, loader, false)
	return it, ok, nil
}

// modulesHint is the upgrade message's sentence for the same facts, or ""
// when no configured module is missing or that is unknown.
func modulesHint(root string, f Facts) string {
	rc, err := ReadRCConf(root)
	if err != nil {
		return ""
	}
	loader, err := readLoader(root)
	if err != nil {
		return ""
	}
	missing, known := NotLoaded(ConfiguredModules(rc, loader), f)
	if !known || len(missing) == 0 {
		return ""
	}
	advice := adviseLoad(missing, loader, f.Kenv, false)
	s := "configured but not loaded: " + strings.Join(missing, " ") + " (" + advice.rebootNote()
	if len(advice.Load) > 0 {
		s += " To load the rest now: " + KldloadCommand(advice.Load)
	}
	return s + "); "
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
	racctPlanned := slices.ContainsFunc(b.plan.Items, func(it Item) bool {
		return it.Kind == Add && it.Action != nil && it.Action.Name == "kern.racct.enable"
	})
	if it, ok := ModulesItem(configured, b.f, b.loader, racctPlanned); ok {
		b.item(it)
	}
}
