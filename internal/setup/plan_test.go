package setup

import (
	"context"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/config"
	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/pkgrepo"
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

// withSkep adds the skep account as the package's pre-install makes it,
// and the skep-cluster account (UID and GID 410) with it.
func withSkep(t *testing.T, root, id string) string {
	t.Helper()
	appendFile(t, filepath.Join(root, "etc/passwd"), "skep:*:"+id+":"+id+":Skep service account:/nonexistent:/usr/sbin/nologin\n")
	appendFile(t, filepath.Join(root, "etc/group"), "skep:*:"+id+":\n")
	withClusterAccount(t, root)
	return root
}

// withClusterAccount adds the skep-cluster account as the package makes it.
func withClusterAccount(t *testing.T, root string) {
	t.Helper()
	appendFile(t, filepath.Join(root, "etc/passwd"), "skep-cluster:*:410:410:Skep cluster service:/nonexistent:/usr/sbin/nologin\n")
	appendFile(t, filepath.Join(root, "etc/group"), "skep-cluster:*:410:\n")
}

// installFirmware puts the files of the edk2-bhyve package the agent needs
// under root.
func installFirmware(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "usr/local/share/edk2-bhyve")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{hostops.FirmwareCodeFile, hostops.FirmwareVarsFile} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("fd"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// stockHost is a stock UFS install with the package installed.
func stockHost(t *testing.T) string {
	t.Helper()
	root := withSkep(t, fixture(t, "stock-ufs"), "409")
	installFirmware(t, root)
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
		`append kern.racct.enable="1" to /boot/loader.conf`,
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
	withClusterAccount(t, root)
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
	wantSteps := []string{"ifconfig bridge0 ifuntagged vtnet1 1", "ifconfig bridge0 defuntagged 1", "ifconfig bridge0 vlanfilter", "ifconfig vtnet1 up",
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

// TestPlanLiveBridges: a bridge is judged as it is now (ifconfig), not only
// by rc.conf (found on smith, 2026-11-22: a new bridge was planned on an
// uplink that was a live member of another bridge, and a hand-made bridge
// passed the management check as "members []").
func TestPlanLiveBridges(t *testing.T) {
	live := func(br Bridge) Facts {
		f := facts()
		f.Interfaces["bridge9"] = nil
		f.Bridges = map[string]Bridge{"bridge9": br}
		return f
	}
	t.Run("new bridge on a live member", func(t *testing.T) {
		p := compute(t, Options{Root: stockHost(t), Mgmt: []string{"vtnet0"}, CreateBridge: "bridge1", Uplink: "vtnet1"},
			live(Bridge{Members: []string{"vtnet1"}}))
		want(t, p, Refuse, "bridge1", "vtnet1 is a member of bridge9 now")
		if !p.Refused() {
			t.Fatal("the plan is not refused")
		}
	})
	t.Run("hand-made bridge with management member", func(t *testing.T) {
		p := compute(t, Options{Root: stockHost(t), Mgmt: []string{"vtnet0"}, Bridges: []string{"bridge9"}},
			live(Bridge{Members: []string{"vtnet1", "vtnet0"}, VLANFilter: true}))
		want(t, p, Refuse, "bridge9", "management interface vtnet0 as a member (now)")
	})
	t.Run("rc.conf member counts too", func(t *testing.T) {
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "etc/rc.conf"), "cloned_interfaces=\"bridge9\"\nifconfig_bridge9=\"addm vtnet0 up\"\n")
		p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}, Bridges: []string{"bridge9"}}, live(Bridge{Members: []string{"vtnet1"}}))
		want(t, p, Refuse, "bridge9", "management interface vtnet0 as a member (in rc.conf)")
	})
	t.Run("hand-made bridge, vlanfilter on now", func(t *testing.T) {
		p := compute(t, Options{Root: stockHost(t), Mgmt: []string{"vtnet0"}, Bridges: []string{"bridge9"}},
			live(Bridge{Members: []string{"vtnet1"}, VLANFilter: true}))
		want(t, p, OK, "bridge9", "members [vtnet1]")
		want(t, p, OK, "bridge9 vlanfilter", "on")
		want(t, p, Warn, "bridge9", "exists only at run time")
	})
	t.Run("hand-made bridge, vlanfilter off now", func(t *testing.T) {
		p := compute(t, Options{Root: stockHost(t), Mgmt: []string{"vtnet0"}, Bridges: []string{"bridge9"}},
			live(Bridge{Members: []string{"vtnet1"}}))
		it := want(t, p, Manual, "bridge9 vlanfilter", "not set")
		if !slices.Contains(it.Steps, "ifconfig bridge9 ifuntagged vtnet1 1") ||
			!slices.Contains(it.Steps, `sysrc ifconfig_bridge9="addm vtnet1 ifuntagged vtnet1 1 defuntagged 1 vlanfilter up"`) {
			t.Fatalf("steps %q", it.Steps)
		}
	})
	t.Run("vlanfilter on now, not in rc.conf", func(t *testing.T) {
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "etc/rc.conf"), "cloned_interfaces=\"bridge9\"\nifconfig_bridge9=\"addm vtnet1 up\"\n")
		p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}, Bridges: []string{"bridge9"}}, live(Bridge{Members: []string{"vtnet1"}, VLANFilter: true}))
		want(t, p, OK, "bridge9 vlanfilter", "on")
		want(t, p, Warn, "bridge9 vlanfilter", "off after a reboot")
	})
	t.Run("vlanfilter in rc.conf, off now", func(t *testing.T) {
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "etc/rc.conf"), "cloned_interfaces=\"bridge9\"\nifconfig_bridge9=\"addm vtnet1 ifuntagged vtnet1 1 defuntagged 1 vlanfilter up\"\n")
		p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}, Bridges: []string{"bridge9"}}, live(Bridge{Members: []string{"vtnet1"}}))
		it := want(t, p, Manual, "bridge9 vlanfilter", "in ifconfig_bridge9 in rc.conf but off now: it takes effect at the next boot")
		wantSteps(t, it, "ifconfig bridge9 ifuntagged vtnet1 1", "ifconfig bridge9 defuntagged 1", "ifconfig bridge9 vlanfilter",
			"ifconfig vtnet1 up", `sysrc ifconfig_vtnet1="up"`)
	})
	t.Run("members differ", func(t *testing.T) {
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "etc/rc.conf"), "cloned_interfaces=\"bridge9\"\nifconfig_bridge9=\"addm vtnet2 vlanfilter up\"\n")
		p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}, Bridges: []string{"bridge9"}}, live(Bridge{Members: []string{"vtnet1"}, VLANFilter: true}))
		want(t, p, OK, "bridge9", "members now [vtnet1], in rc.conf [vtnet2]")
	})
	t.Run("not a bridge", func(t *testing.T) {
		f := live(Bridge{})
		delete(f.Bridges, "bridge9")
		p := compute(t, Options{Root: stockHost(t), Mgmt: []string{"vtnet0"}, Bridges: []string{"bridge9"}}, f)
		want(t, p, Refuse, "bridge9", "not a bridge")
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
		installFirmware(t, root)
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
	// A file setup did not write is the administrator's: parsed, never
	// rewritten; the same allow-lists are fine, others a conflict.
	t.Run("administrator's agent.json, other allow-lists", func(t *testing.T) {
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "usr/local/etc/skep/agent.json"), "{\"storage\":{\"pools\":[\"x\"]}}\n")
		p := compute(t, o(root), facts())
		it := want(t, p, Conflict, agentJSONPath, `storage.pools is ["x"], the answers give ["tank"]`)
		if !strings.Contains(it.Detail, "never rewrites") || !strings.Contains(it.Detail, "Proposed content:\n{") {
			t.Errorf("detail %q", it.Detail)
		}
		if p.Refused() || slices.ContainsFunc(p.Actions(), func(a Action) bool { return a.File == agentJSONPath }) {
			t.Fatal("refused, or plans to write the administrator's agent.json")
		}
	})
	t.Run("administrator's agent.json, same allow-lists", func(t *testing.T) {
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "usr/local/etc/skep/agent.json"), `{"log":{"level":"debug"},
  "network":{"management_interfaces":["vtnet0"],"bridges":[]},"storage":{"pools":["tank"],"directories":[]}}`+"\n")
		want(t, compute(t, o(root), facts()), OK, agentJSONPath, "settings match the answers")
	})
	t.Run("administrator's agent.json, not JSON", func(t *testing.T) {
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "usr/local/etc/skep/agent.json"), "{\"storage\":\n")
		it := want(t, compute(t, o(root), facts()), Refuse, agentJSONPath, "cannot be parsed")
		if !strings.Contains(it.Detail, `"tank"`) {
			t.Error("the proposed version is not printed")
		}
	})
	t.Run("administrator's api.json", func(t *testing.T) {
		root := stockHost(t)
		// No listen_address: skep-api's default, not the answer.
		appendFile(t, filepath.Join(root, "usr/local/etc/skep/api.json"), "{}\n")
		want(t, compute(t, o(root), facts()), Conflict, apiJSONPath, `listen_address is "127.0.0.1:8443", the answers give "192.0.2.10:8443"`)
		os.WriteFile(filepath.Join(root, "usr/local/etc/skep/api.json"), []byte(`{"listen_address":"192.0.2.10:8443","log":{"level":"debug"}}`), 0o640)
		want(t, compute(t, o(root), facts()), OK, apiJSONPath, "settings match the answers")
		os.WriteFile(filepath.Join(root, "usr/local/etc/skep/api.json"), []byte("listen"), 0o640)
		want(t, compute(t, o(root), facts()), Refuse, apiJSONPath, "cannot be parsed")
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
	os.MkdirAll(filepath.Join(root, "srv/group"), 0o770)
	os.Chmod(filepath.Join(root, "srv/group"), 0o770)
	os.MkdirAll(filepath.Join(root, "srv/notroot"), 0o700)
	os.Symlink(filepath.Join(root, "srv/ok"), filepath.Join(root, "srv/link"))
	os.WriteFile(filepath.Join(root, "srv/file"), nil, 0o600)
	// The tests do not run as root: the fixture's directories count as
	// root's, except srv/notroot.
	o := Options{Root: root, Mgmt: []string{"vtnet0"}, DirPools: []DirPool{
		{Name: "a", Path: "/srv/ok"}, {Name: "b", Path: "/srv/missing"}, {Name: "c", Path: "/srv/open"},
		{Name: "d", Path: "/srv/group"}, {Name: "e", Path: "/srv/notroot"}, {Name: "f", Path: "/srv/link"},
		{Name: "g", Path: "/srv/file"}},
		fileOwner: func(st fs.FileInfo) (uint32, bool) {
			if st.Name() == "notroot" {
				return 1001, true
			}
			return 0, true
		}}
	p := compute(t, o, facts())
	want(t, p, OK, "directory pool a", "/srv/ok")
	want(t, p, Manual, "directory pool b", "does not exist")
	want(t, p, Refuse, "directory pool c", "writable by its group or others")
	want(t, p, Refuse, "directory pool d", "writable by its group or others")
	want(t, p, Refuse, "directory pool e", "owned by UID 1001, not root")
	want(t, p, Refuse, "directory pool f", "symbolic link")
	want(t, p, Refuse, "directory pool g", "not a directory")
	// Without the stand-in, the real owner (the test user) is refused.
	o.fileOwner = nil
	if os.Getuid() != 0 {
		want(t, compute(t, o, facts()), Refuse, "directory pool a", "not root")
	}
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
	if it := want(t, p, Warn, "edk2-bhyve", "VMs cannot be created without it"); len(it.Steps) != 1 || it.Steps[0] != "pkg install edk2-bhyve" {
		t.Fatalf("edk2-bhyve item: %+v", it)
	}
	if it := want(t, p, Manual, "FreeBSD-base", "disabled"); len(it.Steps) != 1 || it.Steps[0] != pkgrepo.EnableBase {
		t.Fatalf("FreeBSD-base item: %+v", it)
	}
	want(t, p, Warn, "virtualization", "cannot read")
	appendFile(t, filepath.Join(root, "var/run/dmesg.boot"), "VT-x: PAT,HLT\n")
	want(t, compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}}, facts()), Warn, "virtualization", "VMs will be unavailable")
}

// TestPlanFirmwareFiles: the warning goes by the files the agent opens, not
// by the directory; one missing file is enough, and a link to a file counts.
func TestPlanFirmwareFiles(t *testing.T) {
	root := stockHost(t)
	dir := filepath.Join(root, "usr/local/share/edk2-bhyve")
	want(t, compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}}, facts()), OK, "edk2-bhyve", "")
	if err := os.Remove(filepath.Join(dir, hostops.FirmwareVarsFile)); err != nil {
		t.Fatal(err)
	}
	want(t, compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}}, facts()), Warn, "edk2-bhyve", hostops.FirmwareVarsFile+" missing")
	if err := os.Symlink(hostops.FirmwareCodeFile, filepath.Join(dir, hostops.FirmwareVarsFile)); err != nil {
		t.Fatal(err)
	}
	want(t, compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}}, facts()), OK, "edk2-bhyve", "")
}

// TestFirmwareFilesMatchAgent: setup checks the files the agent needs.
func TestFirmwareFilesMatchAgent(t *testing.T) {
	if len(firmwareFiles) != 2 || firmwareFiles[0] != hostops.FirmwareCodeFile || firmwareFiles[1] != hostops.FirmwareVarsFile {
		t.Fatalf("firmwareFiles %v, agent needs %s and %s", firmwareFiles, hostops.FirmwareCodeFile, hostops.FirmwareVarsFile)
	}
	if firmwareDir != config.DefaultFirmwareDir {
		t.Fatalf("firmwareDir %s, agent default %s", firmwareDir, config.DefaultFirmwareDir)
	}
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

// TestBaseRepoEnabledRoot: the FreeBSD-base check reads the files under
// the root it is given. Compute never hands it an empty root (os.DirFS("")
// fails every read, so it would always say disabled): an empty root is
// refused before anything is read.
func TestBaseRepoEnabledRoot(t *testing.T) {
	if !baseRepoEnabled(stockHost(t)) {
		t.Fatal("stock host with the override: disabled")
	}
	if baseRepoEnabled(fixture(t, "stock-ufs")) {
		t.Fatal("stock host without the override: enabled")
	}
	if _, err := fs.ReadDir(os.DirFS(""), "etc"); err == nil {
		t.Fatal(`os.DirFS("") read a directory; the guard below is moot`)
	}
	if _, err := Compute(context.Background(), Options{Mgmt: []string{"vtnet0"}}, facts()); err == nil || !strings.Contains(err.Error(), "root must not be empty") {
		t.Fatalf("empty root: %v", err)
	}
	p := compute(t, Options{Root: stockHost(t), Mgmt: []string{"vtnet0"}}, facts())
	want(t, p, OK, "FreeBSD-base repository", "enabled")
}

// wantSteps checks an item's steps exactly.
func wantSteps(t *testing.T, it Item, steps ...string) {
	t.Helper()
	if !slices.Equal(it.Steps, steps) {
		t.Fatalf("steps\n got %q\nwant %q", it.Steps, steps)
	}
}

// TestPlanVLANFilterSteps: the steps for an allow-listed bridge without
// vlanfilter. (a) vlanfilter in rc.conf but off now: live steps only, no
// sysrc. (b) a run-time bridge gets a cloned_interfaces step unless
// rc.conf has it. (c) the persisted line names only real uplinks, and a
// name that is not plain gets no steps at all.
func TestPlanVLANFilterSteps(t *testing.T) {
	run := func(t *testing.T, rc string, br *Bridge) Item {
		t.Helper()
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "etc/rc.conf"), rc)
		f := facts()
		if br != nil {
			f.Interfaces["bridge9"] = nil
			f.Bridges = map[string]Bridge{"bridge9": *br}
		}
		p := compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}, Bridges: []string{"bridge9"}}, f)
		for _, it := range p.Items {
			if it.What == "bridge9 vlanfilter" {
				return it
			}
		}
		t.Fatalf("no vlanfilter item: %+v", p.Items)
		return Item{}
	}
	t.Run("a: in rc.conf, off now, with a guest port", func(t *testing.T) {
		it := run(t, "cloned_interfaces=\"bridge9\"\nifconfig_bridge9=\"addm vtnet1 vlanfilter up\"\n",
			&Bridge{Members: []string{"vtnet1", "epair3a"}})
		wantGuestPortRefusal(t, it, "epair3a")
	})
	t.Run("a: in rc.conf, off now, uplink only", func(t *testing.T) {
		it := run(t, "cloned_interfaces=\"bridge9\"\nifconfig_bridge9=\"addm vtnet1 vlanfilter up\"\n",
			&Bridge{Members: []string{"vtnet1"}})
		if it.Kind != Manual || strings.Contains(it.Detail, "rc.conf needs no change") {
			t.Fatalf("%+v", it)
		}
		wantSteps(t, it, "ifconfig bridge9 ifuntagged vtnet1 1", "ifconfig bridge9 defuntagged 1", "ifconfig bridge9 vlanfilter",
			"ifconfig vtnet1 up", `sysrc ifconfig_vtnet1="up"`)
	})
	t.Run("a: the uplink's line already says up: rc.conf needs no change", func(t *testing.T) {
		it := run(t, "cloned_interfaces=\"bridge9\"\nifconfig_vtnet1=\"up\"\nifconfig_bridge9=\"addm vtnet1 vlanfilter up\"\n",
			&Bridge{Members: []string{"vtnet1"}})
		if !strings.Contains(it.Detail, "rc.conf needs no change") {
			t.Fatalf("%+v", it)
		}
		wantSteps(t, it, "ifconfig bridge9 ifuntagged vtnet1 1", "ifconfig bridge9 defuntagged 1", "ifconfig bridge9 vlanfilter", "ifconfig vtnet1 up")
	})
	t.Run("an uplink line without up is never replaced", func(t *testing.T) {
		it := run(t, "ifconfig_vtnet1=\"-lro\"\n", &Bridge{Members: []string{"vtnet1"}})
		for _, s := range it.Steps {
			if strings.Contains(s, "ifconfig_vtnet1") {
				t.Fatalf("replaces the operator's line: %q", it.Steps)
			}
		}
		if !strings.Contains(it.Detail, "Add the word up to the ifconfig_ line of [vtnet1]") {
			t.Fatalf("%+v", it)
		}
	})
	t.Run("b: run-time bridge, not in cloned_interfaces", func(t *testing.T) {
		it := run(t, "", &Bridge{Members: []string{"vtnet1"}})
		wantSteps(t, it, "ifconfig bridge9 ifuntagged vtnet1 1", "ifconfig bridge9 defuntagged 1", "ifconfig bridge9 vlanfilter", "ifconfig vtnet1 up",
			`sysrc cloned_interfaces+="bridge9"`, `sysrc ifconfig_bridge9="addm vtnet1 ifuntagged vtnet1 1 defuntagged 1 vlanfilter up"`, `sysrc ifconfig_vtnet1="up"`)
	})
	t.Run("b: run-time bridge, already in cloned_interfaces", func(t *testing.T) {
		it := run(t, "cloned_interfaces=\"bridge3 bridge9\"\n", &Bridge{Members: []string{"vtnet1"}})
		for _, s := range it.Steps {
			if strings.Contains(s, "cloned_interfaces") {
				t.Fatalf("cloned_interfaces step although rc.conf has it: %q", it.Steps)
			}
		}
	})
	t.Run("run-time bridge with guest ports: no steps", func(t *testing.T) {
		it := run(t, "", &Bridge{Members: []string{"vtnet1", "epair0a", "tap4", "lagg0.60"}})
		wantGuestPortRefusal(t, it, "epair0a", "tap4")
	})
	t.Run("rc.conf bridge with a guest port: no steps", func(t *testing.T) {
		it := run(t, "cloned_interfaces=\"bridge9\"\nifconfig_bridge9=\"addm vtnet1 up\"\n",
			&Bridge{Members: []string{"vtnet1", "epair0a", "vtnet2"}})
		wantGuestPortRefusal(t, it, "epair0a")
	})
	t.Run("guest port with an odd name: no steps, name quoted", func(t *testing.T) {
		it := run(t, "", &Bridge{Members: []string{"vtnet1", "tap$x"}})
		wantGuestPortRefusal(t, it, "tap$x")
	})
	t.Run("c: rc.conf bridge, the line keeps the rc.conf members only", func(t *testing.T) {
		it := run(t, "cloned_interfaces=\"bridge9\"\nifconfig_bridge9=\"addm vtnet1 up\"\n",
			&Bridge{Members: []string{"vtnet1", "vtnet2"}})
		wantSteps(t, it, "ifconfig bridge9 ifuntagged vtnet1 1", "ifconfig bridge9 ifuntagged vtnet2 1",
			"ifconfig bridge9 defuntagged 1", "ifconfig bridge9 vlanfilter", "ifconfig vtnet1 up", "ifconfig vtnet2 up",
			`sysrc ifconfig_bridge9="addm vtnet1 ifuntagged vtnet1 1 defuntagged 1 vlanfilter up"`, `sysrc ifconfig_vtnet1="up"`)
	})
	t.Run("c: rc.conf line with some settings already", func(t *testing.T) {
		it := run(t, "cloned_interfaces=\"bridge9\"\nifconfig_bridge9=\"addm vtnet1 ifuntagged vtnet1 1 up\"\n", &Bridge{Members: []string{"vtnet1"}})
		if !slices.Contains(it.Steps, `sysrc ifconfig_bridge9="addm vtnet1 ifuntagged vtnet1 1 defuntagged 1 vlanfilter up"`) {
			t.Fatalf("steps %q", it.Steps)
		}
	})
	t.Run("rc.conf bridge not created yet: only the line", func(t *testing.T) {
		it := run(t, "cloned_interfaces=\"bridge9\"\nifconfig_bridge9=\"addm vtnet1 up\"\n", nil)
		wantSteps(t, it, `sysrc ifconfig_bridge9="addm vtnet1 ifuntagged vtnet1 1 defuntagged 1 vlanfilter up"`, `sysrc ifconfig_vtnet1="up"`)
	})
	for _, c := range []struct {
		name, rc string
		br       *Bridge
		why      string
	}{
		{"c: odd live member", "", &Bridge{Members: []string{"vtnet1", "we$rd"}}, `member name "we$rd"`},
		{"c: odd rc.conf member", "cloned_interfaces=\"bridge9\"\nifconfig_bridge9=\"addm vt-1 up\"\n", &Bridge{Members: []string{"vtnet1"}}, `member name "vt-1"`},
		{"c: odd rc.conf word", "cloned_interfaces=\"bridge9\"\nifconfig_bridge9=\"addm vtnet1 descr 'a;b' up\"\n", &Bridge{Members: []string{"vtnet1"}}, "has the word"},
		{"a: odd live member", "cloned_interfaces=\"bridge9\"\nifconfig_bridge9=\"addm vtnet1 vlanfilter up\"\n", &Bridge{Members: []string{"vtnet1", "x\"y"}}, "member name"},
	} {
		t.Run(c.name, func(t *testing.T) {
			it := run(t, c.rc, c.br)
			if len(it.Steps) != 0 || !strings.Contains(it.Detail, "setup prints no steps") || !strings.Contains(it.Detail, c.why) {
				t.Fatalf("%+v", it)
			}
		})
	}
}

// wantGuestPortRefusal: a bridge with live guest ports gets no step at
// all, live or sysrc, and the item names the ports and what to do.
func wantGuestPortRefusal(t *testing.T, it Item, ports ...string) {
	t.Helper()
	if it.Kind != Manual || len(it.Steps) != 0 {
		t.Fatalf("want a manual item with no steps: %+v", it)
	}
	for _, w := range []string{"stop the Skep guests on this bridge first, then run plan again", "The agent sets each guest port when the guest starts"} {
		if !strings.Contains(it.Detail, w) {
			t.Fatalf("detail lacks %q: %s", w, it.Detail)
		}
	}
	for _, port := range ports {
		if !strings.Contains(it.Detail, strconv.Quote(port)) {
			t.Fatalf("detail does not name %s: %s", port, it.Detail)
		}
	}
}
