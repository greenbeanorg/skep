package setup

import (
	"context"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/config"
)

// fixture copies testdata/<name> into a fresh root and returns it.
func fixture(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join("testdata", name)
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		dst := filepath.Join(root, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func appendFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
}

// withSkep adds the skep account as the package's pre-install makes it.
func withSkep(t *testing.T, root, id string) string {
	t.Helper()
	appendFile(t, filepath.Join(root, "etc/passwd"), "skep:*:"+id+":"+id+":Skep service account:/nonexistent:/usr/sbin/nologin\n")
	appendFile(t, filepath.Join(root, "etc/group"), "skep:*:"+id+":\n")
	return root
}

// stockHost is a stock UFS install with the package installed.
func stockHost(t *testing.T) string {
	t.Helper()
	root := withSkep(t, fixture(t, "stock-ufs"), "409")
	if err := os.MkdirAll(filepath.Join(root, "usr/local/share/edk2-bhyve"), 0o755); err != nil {
		t.Fatal(err)
	}
	appendFile(t, filepath.Join(root, "var/run/dmesg.boot"), "  VT-x: PAT,HLT,MTF,PAUSE,EPT,UG,VPID\n")
	appendFile(t, filepath.Join(root, "usr/local/etc/pkg/repos/FreeBSD.conf"), "FreeBSD-base: { enabled: yes }\n")
	return root
}

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }

func facts() Facts {
	return Facts{
		KernelRelease: "15.1-RELEASE-p7", UserlandRelease: "15.1-RELEASE-p7", Arch: "amd64",
		Interfaces: map[string][]netip.Prefix{
			"vtnet0": {pfx("192.0.2.10/24")}, "vtnet1": nil, "vtnet2": nil, "lo0": {pfx("127.0.0.1/8")},
		},
		Pools: map[string]string{"zroot": "ONLINE", "tank": "ONLINE", "sick": "DEGRADED"},
	}
}

func compute(t *testing.T, o Options, f Facts) Plan {
	t.Helper()
	p, err := Compute(context.Background(), o, f)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// find returns the first item whose What contains what.
func find(p Plan, what string) (Item, bool) {
	for _, it := range p.Items {
		if strings.Contains(it.What, what) {
			return it, true
		}
	}
	return Item{}, false
}

func want(t *testing.T, p Plan, kind Kind, what, detail string) Item {
	t.Helper()
	for _, it := range p.Items {
		if it.Kind == kind && strings.Contains(it.What, what) && strings.Contains(it.Detail, detail) {
			return it
		}
	}
	var b strings.Builder
	for _, it := range p.Items {
		b.WriteString("  " + string(it.Kind) + " " + it.Area + " " + it.What + " | " + it.Detail + "\n")
	}
	t.Fatalf("no %s item %q with %q in the plan:\n%s", kind, what, detail, b.String())
	return Item{}
}

func actions(p Plan) []string {
	var out []string
	for _, a := range p.Actions() {
		out = append(out, a.String())
	}
	return out
}

func TestPlanStockHostNewBridge(t *testing.T) {
	root := stockHost(t)
	p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}, CreateBridge: "bridge0", Uplink: "vtnet1", Pools: []string{"tank"}}, facts())
	if p.Refused() {
		t.Fatalf("refused: %+v", p.Items)
	}
	got := strings.Join(actions(p), "\n")
	for _, w := range []string{
		`sysrc -f /boot/loader.conf vmm_load="YES"`,
		`sysrc -f /boot/loader.conf nmdm_load="YES"`,
		`sysrc -f /boot/loader.conf kern.racct.enable="1"`,
		`sysrc kld_list+="if_epair netgraph ng_socket ng_ether ng_bpf"`,
		`sysrc zfs_enable="YES"`,
		`sysrc cloned_interfaces+="bridge0"`,
		`sysrc ifconfig_vtnet1="up"`,
		`sysrc ifconfig_bridge0="addm vtnet1 ifuntagged vtnet1 1 defuntagged 1 vlanfilter up"`,
		`sysrc skep_agent_enable="YES"`,
		`sysrc skep_api_enable="YES"`,
		"write /usr/local/etc/skep/agent.json (0600 root:wheel)",
		"write /usr/local/etc/skep/api.json (0640 root:skep)",
		"mkdir /var/db/skep/media (0700 root:wheel)",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("plan lacks %q; actions:\n%s", w, got)
		}
	}
	// DHCP management: accepted with a warning (answer 6), listen on the lease.
	want(t, p, Warn, "management vtnet0", "DHCP")
	if p.Listen != "192.0.2.10:8443" {
		t.Errorf("listen %q", p.Listen)
	}
	want(t, p, Manual, "bridge0 now", "never runs ifconfig")
	want(t, p, OK, "skep", "UID and GID 409")
}

func TestPlanGeneratedConfigLoads(t *testing.T) {
	root := stockHost(t)
	p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}, CreateBridge: "bridge0", Uplink: "vtnet1",
		Pools: []string{"tank"}, DirPools: []DirPool{{Name: "disks", Path: "/var/db/skep-disks"}}}, facts())
	var agent []byte
	for _, a := range p.Actions() {
		if a.File == agentJSONPath {
			agent = a.Content
		}
	}
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, agent, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.LoadOptions{Path: path, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatalf("the agent rejects setup's agent.json: %v\n%s", err, agent)
	}
	if len(cfg.Storage.Pools) != 1 || cfg.Storage.Pools[0] != "tank" || len(cfg.Network.Bridges) != 1 ||
		cfg.Network.Bridges[0] != "bridge0" || cfg.Network.ManagementInterfaces[0] != "vtnet0" ||
		len(cfg.Storage.Directories) != 1 || cfg.Storage.MediaDir != mediaDir {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestPlanNoRACCT(t *testing.T) {
	p := compute(t, Options{Root: stockHost(t), Mgmt: []string{"vtnet0"}, NoRACCT: true}, facts())
	if strings.Contains(strings.Join(actions(p), "\n"), "racct") {
		t.Fatal("--no-racct still adds kern.racct.enable")
	}
	want(t, p, Warn, "kern.racct.enable", "--no-racct")
}

func TestPlanHearst(t *testing.T) {
	root := fixture(t, "hearst")
	f := facts()
	f.Interfaces["vtnet0"] = []netip.Prefix{pfx("10.79.20.170/24")}
	f.Interfaces["bridge0"] = nil
	f.Pools = map[string]string{"skeptest": "ONLINE"}
	p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}, Bridges: []string{"bridge0"}, Pools: []string{"skeptest"}}, f)
	if p.Refused() {
		t.Fatalf("refused: %+v", p.Items)
	}
	want(t, p, OK, "vmm_load", "")
	want(t, p, OK, "kern.racct.enable", "")
	want(t, p, Add, "kld_list+=", "")
	want(t, p, OK, "bridge0", "changes nothing")
	it := want(t, p, Manual, "bridge0 vlanfilter", "PVID first")
	wantSteps := []string{"ifconfig bridge0 ifuntagged vtnet1 1", "ifconfig bridge0 defuntagged 1", "ifconfig bridge0 vlanfilter",
		`sysrc ifconfig_bridge0="addm vtnet1 ifuntagged vtnet1 1 defuntagged 1 vlanfilter up"`}
	if strings.Join(it.Steps, "\n") != strings.Join(wantSteps, "\n") {
		t.Errorf("steps:\n%s", strings.Join(it.Steps, "\n"))
	}
	want(t, p, Warn, "skep", "UID 1002")
	want(t, p, Warn, "skep_api_args", "wins over api.json")
	want(t, p, OK, "skep_agent_enable", "")
	if p.Listen != "10.79.20.170:8443" {
		t.Errorf("listen %q", p.Listen)
	}
	for _, a := range p.Actions() {
		if strings.Contains(a.Name, "bridge0") || a.Name == "cloned_interfaces" {
			t.Errorf("an existing bridge was changed: %s", a)
		}
	}
}

func TestPlanPlatform(t *testing.T) {
	cases := []struct {
		name       string
		kern, user string
		arch       string
		kind       Kind
		detail     string
	}{
		{"arm64", "15.1-RELEASE", "15.1-RELEASE", "aarch64", Refuse, "amd64 only"},
		{"14.3", "14.3-RELEASE", "14.3-RELEASE", "amd64", Refuse, "15.x only"},
		{"mixed", "15.1-RELEASE", "15.0-RELEASE-p3", "amd64", Refuse, "different releases"},
		{"15.0", "15.0-RELEASE-p2", "15.0-RELEASE-p2", "amd64", Warn, "qualified on 15.1"},
		{"15.1 patch levels", "15.1-RELEASE-p7", "15.1-RELEASE-p8", "amd64", OK, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := facts()
			f.KernelRelease, f.UserlandRelease, f.Arch = c.kern, c.user, c.arch
			p := compute(t, Options{Root: stockHost(t), Mgmt: []string{"vtnet0"}}, f)
			for _, it := range p.Items {
				if it.Area == "platform" {
					if it.Kind != c.kind || !strings.Contains(it.Detail, c.detail) {
						t.Fatalf("platform item %+v", it)
					}
					return
				}
			}
			t.Fatal("no platform item")
		})
	}
}

func TestPlanRefusesWithoutRCConf(t *testing.T) {
	root := stockHost(t)
	os.Remove(filepath.Join(root, "etc/rc.conf"))
	p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}}, facts())
	want(t, p, Refuse, "the whole plan", "no rc.conf")
	if len(p.Actions()) != 0 {
		t.Fatal("actions without rc.conf")
	}
}

func TestPlanManagementRefusals(t *testing.T) {
	t.Run("no ifconfig line", func(t *testing.T) {
		f := facts()
		p := compute(t, Options{Root: stockHost(t), Mgmt: []string{"vtnet2"}, Listen: "192.0.2.10:8443"}, f)
		want(t, p, Refuse, "management vtnet2", "will not guess")
	})
	t.Run("no such interface", func(t *testing.T) {
		p := compute(t, Options{Root: stockHost(t), Mgmt: []string{"em0"}, Listen: "192.0.2.10:8443"}, facts())
		want(t, p, Refuse, "management em0", "no such interface")
	})
	t.Run("line without an address", func(t *testing.T) {
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "etc/rc.conf"), "ifconfig_vtnet1=\"up\"\n")
		p := compute(t, Options{Root: root, Mgmt: []string{"vtnet1"}, Listen: "192.0.2.10:8443"}, facts())
		want(t, p, Refuse, "management vtnet1", "neither an address nor DHCP")
	})
	t.Run("static address", func(t *testing.T) {
		root := fixture(t, "hearst")
		f := facts()
		f.Interfaces["vtnet0"] = []netip.Prefix{pfx("10.79.20.170/24")}
		p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}}, f)
		want(t, p, OK, "management vtnet0", "never changes it")
	})
	t.Run("SYNCDHCP warns", func(t *testing.T) {
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "etc/rc.conf"), "ifconfig_vtnet0=\"SYNCDHCP\"\n")
		p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}}, facts())
		want(t, p, Warn, "management vtnet0", "DHCP")
	})
}

func TestPlanListen(t *testing.T) {
	t.Run("0.0.0.0 refused", func(t *testing.T) {
		p := compute(t, Options{Root: stockHost(t), Mgmt: []string{"vtnet0"}, Listen: "0.0.0.0:8443"}, facts())
		want(t, p, Refuse, "listen 0.0.0.0:8443", "--listen-all")
	})
	t.Run(":: refused", func(t *testing.T) {
		p := compute(t, Options{Root: stockHost(t), Mgmt: []string{"vtnet0"}, Listen: "[::]:8443"}, facts())
		want(t, p, Refuse, "listen [::]:8443", "--listen-all")
	})
	t.Run("--listen-all", func(t *testing.T) {
		p := compute(t, Options{Root: stockHost(t), Mgmt: []string{"vtnet0"}, Listen: "0.0.0.0:8443", ListenAll: true}, facts())
		if p.Refused() || p.Listen != "0.0.0.0:8443" {
			t.Fatalf("listen %q refused %v", p.Listen, p.Refused())
		}
	})
	t.Run("no address now", func(t *testing.T) {
		f := facts()
		f.Interfaces["vtnet0"] = nil
		p := compute(t, Options{Root: stockHost(t), Mgmt: []string{"vtnet0"}}, f)
		want(t, p, Refuse, "listen address", "--listen")
	})
}

func TestPlanExistingBridgeRefusals(t *testing.T) {
	t.Run("management member", func(t *testing.T) {
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "etc/rc.conf"), "cloned_interfaces=\"bridge0\"\nifconfig_bridge0=\"addm vtnet0 addm vtnet1 up\"\n")
		p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}, Bridges: []string{"bridge0"}}, facts())
		want(t, p, Refuse, "bridge0", "management interface vtnet0")
	})
	t.Run("does not exist", func(t *testing.T) {
		p := compute(t, Options{Root: stockHost(t), Mgmt: []string{"vtnet0"}, Bridges: []string{"bridge7"}}, facts())
		want(t, p, Refuse, "bridge7", "does not exist")
	})
	t.Run("vlanfilter on", func(t *testing.T) {
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "etc/rc.conf"), "cloned_interfaces=\"bridge0\"\nifconfig_bridge0=\"addm vtnet1 ifuntagged vtnet1 1 defuntagged 1 vlanfilter up\"\n")
		p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}, Bridges: []string{"bridge0"}}, facts())
		want(t, p, OK, "bridge0 vlanfilter", "on")
	})
}

func TestPlanNewBridgeRefusals(t *testing.T) {
	cases := []struct {
		name, rc, uplink, detail string
		live                     []netip.Prefix
	}{
		{"uplink is management", "", "vtnet0", "is a management interface", nil},
		{"uplink has a live address", "", "vtnet1", "has an address now", []netip.Prefix{pfx("198.51.100.2/24")}},
		{"uplink has an address in rc.conf", "ifconfig_vtnet1=\"inet 198.51.100.2/24\"\n", "vtnet1", "address in rc.conf", nil},
		{"uplink on DHCP in rc.conf", "ifconfig_vtnet1=\"DHCP\"\n", "vtnet1", "address in rc.conf", nil},
		{"uplink has other settings", "ifconfig_vtnet1=\"mtu 9000 up\"\n", "vtnet1", "already has settings", nil},
		{"uplink has an alias", "ifconfig_vtnet1_alias0=\"inet 198.51.100.3/32\"\n", "vtnet1", "other settings", nil},
		{"uplink in another bridge", "cloned_interfaces=\"bridge1\"\nifconfig_bridge1=\"addm vtnet1 up\"\n", "vtnet1", "member of bridge1", nil},
		{"uplink missing", "", "em9", "does not exist", nil},
		{"bridge exists", "cloned_interfaces=\"bridge0\"\n", "vtnet1", "exists already", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := stockHost(t)
			appendFile(t, filepath.Join(root, "etc/rc.conf"), c.rc)
			f := facts()
			if c.live != nil {
				f.Interfaces[c.uplink] = c.live
			}
			p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}, CreateBridge: "bridge0", Uplink: c.uplink}, f)
			want(t, p, Refuse, "bridge0", c.detail)
			for _, a := range p.Actions() {
				if a.Name == "cloned_interfaces" || strings.HasPrefix(a.Name, "ifconfig_") {
					t.Fatalf("plans a bridge change while refusing: %s", a)
				}
			}
		})
	}
	t.Run("uplink already up", func(t *testing.T) {
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "etc/rc.conf"), "ifconfig_vtnet1=\"up\"\n")
		p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}, CreateBridge: "bridge0", Uplink: "vtnet1"}, facts())
		if p.Refused() {
			t.Fatal("refused an uplink that is only up")
		}
		if strings.Contains(strings.Join(actions(p), "\n"), `ifconfig_vtnet1="up"`) {
			t.Fatal("re-adds ifconfig_vtnet1")
		}
	})
}

func TestPlanAccountRefusals(t *testing.T) {
	base := func(t *testing.T) string {
		root := fixture(t, "stock-ufs")
		os.MkdirAll(filepath.Join(root, "usr/local/share/edk2-bhyve"), 0o755)
		return root
	}
	t.Run("only the user", func(t *testing.T) {
		root := base(t)
		appendFile(t, filepath.Join(root, "etc/passwd"), "skep:*:409:409::/nonexistent:/usr/sbin/nologin\n")
		want(t, compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}}, facts()), Refuse, "skep", "only one")
	})
	t.Run("only the group", func(t *testing.T) {
		root := base(t)
		appendFile(t, filepath.Join(root, "etc/group"), "skep:*:409:\n")
		want(t, compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}}, facts()), Refuse, "skep", "only one")
	})
	t.Run("missing", func(t *testing.T) {
		want(t, compute(t, Options{Root: base(t), Mgmt: []string{"vtnet0"}}, facts()), Refuse, "skep", "reinstall the package")
	})
	t.Run("missing and 409 taken", func(t *testing.T) {
		root := base(t)
		appendFile(t, filepath.Join(root, "etc/passwd"), "evcc:*:409:409::/nonexistent:/usr/sbin/nologin\n")
		want(t, compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}}, facts()), Refuse, "skep", "UID 409 belongs to evcc")
	})
	t.Run("login shell", func(t *testing.T) {
		root := base(t)
		appendFile(t, filepath.Join(root, "etc/passwd"), "skep:*:409:409::/home/skep:/bin/sh\n")
		appendFile(t, filepath.Join(root, "etc/group"), "skep:*:409:\n")
		want(t, compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}}, facts()), Refuse, "skep", "no login shell")
	})
}

func TestPlanConfigFiles(t *testing.T) {
	o := func(root string) Options {
		return Options{Root: root, Mgmt: []string{"vtnet0"}, Pools: []string{"tank"}}
	}
	t.Run("administrator's agent.json", func(t *testing.T) {
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "usr/local/etc/skep/agent.json"), "{\"storage\":{\"pools\":[\"x\"]}}\n")
		it := want(t, compute(t, o(root), facts()), Refuse, agentJSONPath, "not written by skep-setup")
		if !strings.Contains(it.Detail, `"tank"`) {
			t.Error("the proposed version is not printed")
		}
	})
	t.Run("administrator's api.json", func(t *testing.T) {
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "usr/local/etc/skep/api.json"), "{}\n")
		want(t, compute(t, o(root), facts()), Refuse, apiJSONPath, "not written by skep-setup")
	})
	t.Run("same content", func(t *testing.T) {
		root := stockHost(t)
		p := compute(t, o(root), facts())
		for _, a := range p.Actions() {
			if a.Type == ActWrite {
				appendFile(t, hostPath(root, a.File), string(a.Content))
			}
		}
		p = compute(t, o(root), facts())
		want(t, p, OK, agentJSONPath, "as setup would write it")
		want(t, p, OK, apiJSONPath, "as setup would write it")
	})
	t.Run("written by setup, other answers", func(t *testing.T) {
		root := stockHost(t)
		content := "{\"old\":true}\n"
		appendFile(t, hostPath(root, agentJSONPath), content)
		writeJournal(t, root, Journal{Format: JournalFormat, ID: "20261101T000000Z", Command: "apply", Complete: true,
			Entries: []Entry{{Action: Action{Type: ActWrite, File: agentJSONPath}, File: agentJSONPath, AfterSHA256: sha256Hex([]byte(content)), Done: true}}})
		want(t, compute(t, o(root), facts()), Conflict, agentJSONPath, "never rewrites it")
	})
}

func writeJournal(t *testing.T, root string, j Journal) {
	t.Helper()
	if err := os.MkdirAll(JournalDir(root, j.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := j.save(root); err != nil {
		t.Fatal(err)
	}
}

func TestPlanRefusesAfterIncompleteJournal(t *testing.T) {
	root := stockHost(t)
	writeJournal(t, root, Journal{Format: JournalFormat, ID: "20261122T100000Z", Command: "apply"})
	want(t, compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}}, facts()), Refuse, "apply 20261122T100000Z", "rollback")
}

func TestPlanPools(t *testing.T) {
	p := compute(t, Options{Root: stockHost(t), Mgmt: []string{"vtnet0"}, Pools: []string{"nope", "sick"}}, facts())
	want(t, p, Refuse, "pool nope", "never creates or imports")
	want(t, p, Refuse, "pool sick", "DEGRADED")
	zfs := fixture(t, "stock-zfs")
	withSkep(t, zfs, "409")
	p = compute(t, Options{Root: zfs, Mgmt: []string{"vtnet0"}, Pools: []string{"zroot"}}, facts())
	want(t, p, OK, "zfs_enable", "already YES")
	want(t, p, OK, "pool zroot", "ONLINE")
}

func TestPlanDirectoryPools(t *testing.T) {
	root := stockHost(t)
	os.MkdirAll(filepath.Join(root, "srv/ok"), 0o700)
	os.MkdirAll(filepath.Join(root, "srv/open"), 0o777)
	os.Chmod(filepath.Join(root, "srv/open"), 0o777)
	p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}, DirPools: []DirPool{
		{Name: "a", Path: "/srv/ok"}, {Name: "b", Path: "/srv/missing"}, {Name: "c", Path: "/srv/open"}}}, facts())
	want(t, p, OK, "directory pool a", "/srv/ok")
	want(t, p, Manual, "directory pool b", "does not exist")
	want(t, p, Refuse, "directory pool c", "writable by others")
}

func TestPlanUnparsableFiles(t *testing.T) {
	t.Run("shell code in rc.conf", func(t *testing.T) {
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "etc/rc.conf"), "if [ -f /x ]; then foo=1; fi\n")
		p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}}, facts())
		want(t, p, Manual, "any edit", "not plain assignments")
		for _, a := range p.Actions() {
			if a.Type == ActSysrc && a.File == rcConfPath {
				t.Fatalf("edits an unparsable rc.conf: %s", a)
			}
		}
		want(t, p, Manual, "kld_list+=", "")
		want(t, p, Add, "vmm_load", "") // loader.conf is still plain
	})
	t.Run("substitution", func(t *testing.T) {
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "boot/loader.conf"), "foo=\"$(cat /x)\"\n")
		p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}}, facts())
		want(t, p, Manual, "vmm_load", "")
	})
	t.Run("rc.conf.d overrides kld_list", func(t *testing.T) {
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "etc/rc.conf.d/kld"), "kld_list=\"if_epair\"\n")
		p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}}, facts())
		want(t, p, Manual, "kld_list", "rc.conf.d/kld")
	})
	t.Run("rc.conf.local with shell code", func(t *testing.T) {
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "etc/rc.conf.local"), ". /etc/other\n")
		p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}}, facts())
		want(t, p, Manual, "any edit", "rc.conf.local")
	})
	t.Run("operator's value", func(t *testing.T) {
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "boot/loader.conf"), "vmm_load=\"NO\"\n")
		appendFile(t, filepath.Join(root, "etc/rc.conf"), "skep_api_enable=\"NO\"\n")
		p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}}, facts())
		want(t, p, Conflict, "vmm_load", `"NO"`)
		want(t, p, Conflict, "skep_api_enable", `"NO"`)
	})
}

func TestPlanReadOnlyChecks(t *testing.T) {
	root := fixture(t, "stock-ufs")
	withSkep(t, root, "409")
	p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}}, facts())
	want(t, p, Manual, "edk2-bhyve", "")
	want(t, p, Manual, "FreeBSD-base", "disabled")
	want(t, p, Warn, "virtualization", "cannot read")
	appendFile(t, filepath.Join(root, "var/run/dmesg.boot"), "VT-x: PAT,HLT\n")
	want(t, compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}}, facts()), Warn, "virtualization", "VMs will be unavailable")
}

func TestPlanGuests(t *testing.T) {
	f := facts()
	f.Jails = []string{"skep-1", "www"}
	f.VMs = []string{"skep-2", "legacy"}
	p := compute(t, Options{Root: stockHost(t), Mgmt: []string{"vtnet0"}}, f)
	want(t, p, OK, "1 Skep jail(s), 1 Skep VM(s)", "never runs ifconfig")
	want(t, p, Warn, "not managed by Skep", "www")
}

func TestPlanStartAndAdmin(t *testing.T) {
	p := compute(t, Options{Root: stockHost(t), Mgmt: []string{"vtnet0"}, Start: true, Admin: "andy"}, facts())
	got := strings.Join(actions(p), "\n")
	for _, w := range []string{"service skep_agent start", "service skep_api start", "skep-api admin create --username andy"} {
		if !strings.Contains(got, w) {
			t.Errorf("lacks %q", w)
		}
	}
	if strings.Index(got, "skep_agent start") > strings.Index(got, "skep_api start") {
		t.Error("the API starts before the agent")
	}
}

func TestValidateOptions(t *testing.T) {
	bad := []Options{
		{Root: "/"},
		{Root: "/", Mgmt: []string{"vtnet0;rm"}},
		{Root: "/", Mgmt: []string{"vtnet0"}, Bridges: []string{"br0"}},
		{Root: "/", Mgmt: []string{"vtnet0"}, CreateBridge: "bridge0"},
		{Root: "/", Mgmt: []string{"vtnet0"}, Uplink: "vtnet1"},
		{Root: "/", Mgmt: []string{"vtnet0"}, CreateBridge: "bridge0", Uplink: "vtnet1", Bridges: []string{"bridge0"}},
		{Root: "/", Mgmt: []string{"vtnet0"}, Pools: []string{"-o x"}},
		{Root: "/", Mgmt: []string{"vtnet0"}, DirPools: []DirPool{{Name: "d", Path: "relative"}}},
		{Root: "/", Mgmt: []string{"vtnet0"}, Admin: "Andy Smith"},
		{Root: "/", Mgmt: []string{"vtnet0"}, Listen: "host.example:8443"},
		{Root: "/", Mgmt: []string{"vtnet0"}, Listen: "10.0.0.1:99999"},
		{Mgmt: []string{"vtnet0"}},
	}
	for i, o := range bad {
		if err := ValidateOptions(o); err == nil {
			t.Errorf("case %d accepted: %+v", i, o)
		}
	}
}
