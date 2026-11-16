// Package templates is the agent's jail template service (ADR 0010
// decision 7 and the step 6 decision on package scripts): the compiled
// allowlist of package scripts and trigger files, the build sequence, the
// container.template.create and delete tasks, and the start-up sweep of
// leftover build jails. Every host operation goes through hostops.
package templates

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Script kinds, as pkg info --raw names them.
const (
	KindScripts    = "scripts"
	KindLuaScripts = "lua_scripts"
)

// ScriptEntry allows one package script: by package name, kind, phase,
// and the SHA-256 of its body as pkg info --raw gives it (the UTF-8
// string, no newline added). The version is for reference only.
type ScriptEntry struct {
	Package string
	Kind    string
	Phase   string
	SHA256  string
	// Commands are the build commands that do this script's work.
	Commands []hostops.BuildCommand
	// Version is the package version the hash was taken from.
	Version string
}

// TriggerEntry allows one trigger definition file, by its path in the
// template and the SHA-256 of its bytes.
type TriggerEntry struct {
	Path     string
	SHA256   string
	Commands []hostops.BuildCommand
	// Package is the package that ships the file, for reference.
	Package string
}

// Allowlist is what a template of one release may contain. It is compiled
// in, not configuration, because it is a security boundary.
type Allowlist struct {
	Release  string
	Scripts  []ScriptEntry
	Triggers []TriggerEntry
}

// allowlists are keyed by release major.minor. The 15.1 entries are those
// of ADR 0010, recorded from the traced build and the probe on hearst.
var allowlists = map[string]Allowlist{
	"15.1": {
		Release: "15.1",
		Scripts: []ScriptEntry{
			{Package: "FreeBSD-caroot", Kind: KindScripts, Phase: "post-install",
				SHA256:   "5976bb9bdaf0cbb34807cdd7eb9b1b30e4913f92f4c2422c96caea0b381c8099",
				Commands: []hostops.BuildCommand{hostops.BuildCertctl}, Version: "15.1p6"},
			// The same body runs at deinstall; Skep never deinstalls from
			// a template, so no command maps to it.
			{Package: "FreeBSD-caroot", Kind: KindScripts, Phase: "post-deinstall",
				SHA256: "5976bb9bdaf0cbb34807cdd7eb9b1b30e4913f92f4c2422c96caea0b381c8099", Version: "15.1p6"},
			{Package: "FreeBSD-runtime", Kind: KindScripts, Phase: "post-install",
				SHA256:   "900cf7d3a93efcb3e894d1517d9d1c913932041ee97ad339adeed6f4ae9ce0f9",
				Commands: []hostops.BuildCommand{hostops.BuildPwdMkdb, hostops.BuildServicesMkdb}, Version: "15.1p6"},
			{Package: "FreeBSD-utilities", Kind: KindScripts, Phase: "post-install",
				SHA256:   "8620cabb4dfe8249fc0fe50ff62fc6e0cd4df957ac46b112098baa4ee63c698f",
				Commands: []hostops.BuildCommand{hostops.BuildCapMkdb}, Version: "15.1p4"},
		},
		Triggers: []TriggerEntry{
			{Path: "usr/share/pkg/triggers/mandoc.ucl",
				SHA256:   "c6cc94249864d251d0a143b62c21019d5f4ab593299c55dccdb5f0574fdb1590",
				Commands: []hostops.BuildCommand{hostops.BuildMakewhatisMan, hostops.BuildMakewhatisOpenSSL},
				Package:  "FreeBSD-mandoc"},
		},
	},
}

// AllowlistFor returns the allowlist of a release's major.minor.
func AllowlistFor(majorMinor string) (Allowlist, bool) {
	a, ok := allowlists[majorMinor]
	return a, ok
}

// Release is a parsed FreeBSD release, for example 15.1-RELEASE-p6.
type Release struct {
	Major, Minor int
	// Branch is RELEASE, STABLE, BETA2, ...
	Branch string
	// Patch is the -pN level, 0 without one.
	Patch int
}

// ParseRelease parses freebsd-version's output.
func ParseRelease(s string) (Release, error) {
	if !hostops.ValidRelease(s) {
		return Release{}, fmt.Errorf("templates: %q is not a FreeBSD release", s)
	}
	mm, rest, _ := strings.Cut(s, "-")
	maj, min, _ := strings.Cut(mm, ".")
	var r Release
	r.Major, _ = strconv.Atoi(maj)
	r.Minor, _ = strconv.Atoi(min)
	branch, patch, hasPatch := strings.Cut(rest, "-p")
	r.Branch = branch
	if hasPatch {
		r.Patch, _ = strconv.Atoi(patch)
	}
	return r, nil
}

// MajorMinor is "15.1".
func (r Release) MajorMinor() string { return strconv.Itoa(r.Major) + "." + strconv.Itoa(r.Minor) }

// checkRelease applies the release rule (decision 7b and the step 6
// decision, item 6): a template's userland must have the host kernel's
// major.minor. Only major.minor is compared; the patch level may differ in
// either direction and is reported, not refused.
func checkRelease(template, host string) error {
	t, err := ParseRelease(template)
	if err != nil {
		return releaseUnsupported(fmt.Sprintf("the template's userland release %q cannot be read", template), template, host)
	}
	h, err := ParseRelease(host)
	if err != nil {
		return err
	}
	if t.MajorMinor() != h.MajorMinor() {
		return releaseUnsupported(fmt.Sprintf("the template's userland is %s and the host runs %s; a template must have the host's release (%s)",
			template, host, h.MajorMinor()), template, host)
	}
	return nil
}

// PatchDiffers reports whether two releases of the same major.minor have
// different patch levels.
func PatchDiffers(template, host string) bool {
	t, err1 := ParseRelease(template)
	h, err2 := ParseRelease(host)
	return err1 == nil && err2 == nil && t.MajorMinor() == h.MajorMinor() && (t.Patch != h.Patch || t.Branch != h.Branch)
}

func releaseUnsupported(msg, template, host string) error {
	return &models.Error{Code: models.CodeReleaseUnsupported, Message: msg,
		Details: map[string]any{"template_release": template, "host_release": host}}
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// maxScriptText bounds the script text put into an error.
const maxScriptText = 8 << 10

func clip(s string) string {
	if len(s) > maxScriptText {
		return s[:maxScriptText] + "\n[... cut]"
	}
	return s
}

func scriptUnknown(msg string, details map[string]any) error {
	return &models.Error{Code: models.CodeTemplateScriptUnknown, Message: msg, Details: details}
}

// CheckScripts checks every script of every installed package against
// the allowlist. The first one not on it fails with
// template_script_unknown, naming the package and phase and carrying the
// script's text. It returns the build commands the allowed scripts call
// for.
func (a Allowlist) CheckScripts(pkgs []hostops.PackageScripts) ([]hostops.BuildCommand, error) {
	allowed := map[string]ScriptEntry{}
	for _, e := range a.Scripts {
		allowed[e.Package+"\x00"+e.Kind+"\x00"+e.Phase+"\x00"+e.SHA256] = e
	}
	var cmds []hostops.BuildCommand
	sorted := append([]hostops.PackageScripts(nil), pkgs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, p := range sorted {
		check := func(kind, phase, body string) error {
			h := sum([]byte(body))
			e, ok := allowed[p.Name+"\x00"+kind+"\x00"+phase+"\x00"+h]
			if !ok {
				return scriptUnknown(fmt.Sprintf("the package %s %s has a %s %s script that is not on Skep's allowlist for %s",
					p.Name, p.Version, phase, kind, a.Release),
					map[string]any{"package": p.Name, "version": p.Version, "kind": kind, "phase": phase,
						"sha256": h, "text": clip(body)})
			}
			cmds = append(cmds, e.Commands...)
			return nil
		}
		for _, phase := range sortedKeys(p.Scripts) {
			if err := check(KindScripts, phase, p.Scripts[phase]); err != nil {
				return nil, err
			}
		}
		for _, phase := range sortedKeys(p.LuaScripts) {
			for _, body := range p.LuaScripts[phase] {
				if err := check(KindLuaScripts, phase, body); err != nil {
					return nil, err
				}
			}
		}
	}
	return cmds, nil
}

// CheckTriggers checks every entry of the template's trigger directories.
// A symbolic link or anything else that is not a regular file fails, and
// so does a file whose (path, SHA-256) is not on the allowlist; the error
// names the file and carries its content. It returns the build commands
// the allowed triggers call for.
func (a Allowlist) CheckTriggers(files []hostops.TriggerFile) ([]hostops.BuildCommand, error) {
	allowed := map[string]TriggerEntry{}
	for _, e := range a.Triggers {
		allowed[e.Path+"\x00"+e.SHA256] = e
	}
	var cmds []hostops.BuildCommand
	for _, f := range files {
		if !f.Regular {
			return nil, scriptUnknown("the trigger directory entry "+f.Path+" is not a regular file",
				map[string]any{"path": f.Path})
		}
		h := sum(f.Content)
		e, ok := allowed[f.Path+"\x00"+h]
		if !ok {
			return nil, scriptUnknown(fmt.Sprintf("the trigger file %s is not on Skep's allowlist for %s", f.Path, a.Release),
				map[string]any{"path": f.Path, "sha256": h, "text": clip(string(f.Content))})
		}
		cmds = append(cmds, e.Commands...)
	}
	return cmds, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// orderCommands returns the commands asked for, once each, in the fixed
// order of hostops.BuildCommands.
func orderCommands(asked ...[]hostops.BuildCommand) []hostops.BuildCommand {
	want := map[hostops.BuildCommand]bool{}
	for _, list := range asked {
		for _, c := range list {
			want[c] = true
		}
	}
	var out []hostops.BuildCommand
	for _, c := range hostops.BuildCommands {
		if want[c] {
			out = append(out, c)
		}
	}
	return out
}
