package hostops

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

const ctrA = "c0c0c0c0-1111-4222-8333-444455556666"

func jailSpec() JailSpec {
	return JailSpec{Pool: "skeptest", ContainerID: ctrA, Hostname: "web1.lab", StartTimeout: 120 * time.Second, StopTimeout: 10 * time.Second}
}

func TestJailNames(t *testing.T) {
	c := testConf(t)
	c.JailRunDir = "/var/run/skep/jails"
	if ds, err := c.JailDataset("skeptest", ctrA); err != nil || ds != "skeptest/skep/jail/"+ctrA {
		t.Fatalf("JailDataset = %q, %v", ds, err)
	}
	if ds, err := c.JailsDataset("skeptest"); err != nil || ds != "skeptest/skep/jail" {
		t.Fatalf("JailsDataset = %q, %v", ds, err)
	}
	if n, err := JailName(ctrA); err != nil || n != "skep-"+ctrA {
		t.Fatalf("JailName = %q, %v", n, err)
	}
	if _, err := JailName("../x"); err == nil {
		t.Fatal("an invalid container ID made a jail name")
	}
	if p, err := c.JailRunPath(ctrA, JailConfFile); err != nil || p != "/var/run/skep/jails/"+ctrA+"/jail.conf" {
		t.Fatalf("JailRunPath = %q, %v", p, err)
	}
	if _, err := c.JailRunPath(ctrA, "../rc.conf"); err == nil {
		t.Fatal("an arbitrary runtime file name was accepted")
	}
	if _, err := (Confinement{Pools: []string{"skeptest"}, DatasetRoot: "skep"}).JailRunPath(ctrA, JailConfFile); err == nil {
		t.Fatal("an unconfigured runtime directory was accepted")
	}
}

func TestValidHostname(t *testing.T) {
	for _, ok := range []string{"web1", "web1.lab", "a", "x-y.example.org", strings.Repeat("a", 63)} {
		if !ValidHostname(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "-a", "a-", "a..b", "a b", "a\"b", "a;b", "a$b", strings.Repeat("a", 64), "a_b", strings.Repeat("a.", 127) + "aa"} {
		if ValidHostname(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRenderJailConf(t *testing.T) {
	conf, err := renderJailConf(jailSpec(), "/skeptest/skep/jail/"+ctrA, "/var/run/skep/jails/"+ctrA+"/console.log")
	if err != nil {
		t.Fatal(err)
	}
	s := string(conf)
	for _, want := range []string{
		"skep-" + ctrA + " {\n",
		"\tpath = \"/skeptest/skep/jail/" + ctrA + "\";\n",
		"\thost.hostname = \"web1.lab\";\n",
		"\tvnet;\n", "\tpersist;\n", "\tenforce_statfs = 2;\n", "\tchildren.max = 0;\n", "\tsecurelevel = 2;\n",
		"\tmount.devfs;\n", "\tdevfs_ruleset = 5;\n",
		"\tallow.suser = 1;\n", "\tallow.reserved_ports = 1;\n",
		"\tallow.set_hostname = 0;\n", "\tallow.unprivileged_proc_debug = 0;\n", "\tallow.raw_sockets = 0;\n",
		"\tallow.mount = 0;\n", "\tallow.mlock = 0;\n", "\tallow.chflags = 0;\n", "\tallow.sysvipc = 0;\n",
		"\tsysvshm = disable;\n", "\tsysvsem = disable;\n", "\tsysvmsg = disable;\n",
		"\texec.clean;\n", "\texec.start = \"/bin/sh /etc/rc\";\n", "\texec.stop = \"/bin/sh /etc/rc.shutdown jail\";\n",
		"\texec.consolelog = \"/var/run/skep/jails/" + ctrA + "/console.log\";\n",
		"\texec.timeout = 120;\n", "\tstop.timeout = 10;\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("jail.conf lacks %q", want)
		}
	}
	// Never a host-side command, and nothing but the two fixed exec.* commands.
	for _, never := range []string{"exec.prestart", "exec.poststart", "exec.prestop", "exec.poststop", "exec.created",
		"mount.fstab", "ip4", "ip6", "allow.vmm", "command"} {
		if strings.Contains(s, never) {
			t.Errorf("jail.conf contains %q", never)
		}
	}
	if n := strings.Count(s, "allow."); n != len(buildJailAllow) {
		t.Errorf("%d allow.* lines, want %d", n, len(buildJailAllow))
	}

	spec := jailSpec()
	spec.SysVIPC, spec.Mlock = true, true
	conf, _ = renderJailConf(spec, "/r", "/l")
	for _, want := range []string{"\tsysvshm = new;\n", "\tsysvsem = new;\n", "\tsysvmsg = new;\n", "\tallow.mlock = 1;\n", "\tallow.sysvipc = 0;\n"} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("opt-in jail.conf lacks %q", want)
		}
	}

	for name, mutate := range map[string]func(*JailSpec){
		"hostname with a quote": func(s *JailSpec) { s.Hostname = `a";exec.prestart="x` },
		"empty hostname":        func(s *JailSpec) { s.Hostname = "" },
		"bad ID":                func(s *JailSpec) { s.ContainerID = "x" },
		"zero stop timeout":     func(s *JailSpec) { s.StopTimeout = 0 },
		"long start timeout":    func(s *JailSpec) { s.StartTimeout = time.Hour },
		"fractional timeout":    func(s *JailSpec) { s.StopTimeout = 1500 * time.Millisecond },
	} {
		s := jailSpec()
		mutate(&s)
		if _, err := renderJailConf(s, "/r", "/l"); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
	if _, err := renderJailConf(jailSpec(), `/r";exec.prestart="/bin/sh`, "/l"); err == nil {
		t.Error("a path with a quote was rendered")
	}
}

// goodParams is what jls --libxo=json -n reports for a jail started from
// the baseline (the shapes recorded on hearst: numbers as strings,
// booleans as booleans).
func goodParams(root string) map[string]any {
	p := map[string]any{
		"name": "skep-" + ctrA, "path": root, "host.hostname": "web1.lab", "vnet": "new", "persist": true,
		"enforce_statfs": "2", "children.max": "0", "securelevel": "2", "devfs_ruleset": "5", "jid": "12",
		"sysvshm": "disable", "sysvsem": "disable", "sysvmsg": "disable",
	}
	for _, a := range append(buildJailAllow, "mount.devfs", "mount.procfs", "mount.tmpfs", "mount.zfs", "vmm", "vmm_ppt") {
		p["allow."+a] = a == "suser" || a == "reserved_ports"
	}
	return p
}

func TestCheckJailParams(t *testing.T) {
	root := "/skeptest/skep/jail/" + ctrA
	if err := checkJailParams(goodParams(root), jailSpec(), root); err != nil {
		t.Fatalf("the baseline was refused: %v", err)
	}
	for name, c := range map[string]struct {
		key  string
		val  any
		want string
	}{
		"reserved ports off":  {"allow.reserved_ports", false, "allow.reserved_ports=false (want true)"},
		"suser off":           {"allow.suser", false, "allow.suser=false (want true)"},
		"set_hostname on":     {"allow.set_hostname", true, "allow.set_hostname=true (want false)"},
		"proc debug on":       {"allow.unprivileged_proc_debug", true, "allow.unprivileged_proc_debug=true (want false)"},
		"a module's allow on": {"allow.vmm", true, "allow.vmm=true (want false)"},
		"not vnet":            {"vnet", "inherit", "vnet=inherit (want new)"},
		"securelevel":         {"securelevel", "0", "securelevel=0 (want 2)"},
		"devfs ruleset":       {"devfs_ruleset", "0", "devfs_ruleset=0 (want 5)"},
		"sysv":                {"sysvshm", "inherit", "sysvshm=inherit (want disable)"},
		"other path":          {"path", "/", "path=/ (want " + root + ")"},
		"allow as a string":   {"allow.mount", "0", "allow.mount=0 (want false)"},
	} {
		p := goodParams(root)
		p[c.key] = c.val
		err := checkJailParams(p, jailSpec(), root)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	p := goodParams(root)
	delete(p, "allow.suser")
	if err := checkJailParams(p, jailSpec(), root); err == nil || !strings.Contains(err.Error(), "allow.suser not reported") {
		t.Errorf("missing allow.suser: %v", err)
	}
	spec := jailSpec()
	spec.Mlock, spec.SysVIPC = true, true
	p = goodParams(root)
	p["allow.mlock"] = true
	p["sysvshm"], p["sysvsem"], p["sysvmsg"] = "new", "new", "new"
	if err := checkJailParams(p, spec, root); err != nil {
		t.Errorf("the opt-ins were refused: %v", err)
	}
}

func sshKey(typ string, body []byte) string {
	blob := make([]byte, 4, 4+len(typ)+4+len(body))
	binary.BigEndian.PutUint32(blob, uint32(len(typ)))
	blob = append(blob, typ...)
	blob = binary.BigEndian.AppendUint32(blob, uint32(len(body)))
	blob = append(blob, body...)
	return typ + " " + base64.StdEncoding.EncodeToString(blob)
}

func TestNormalizeSSHPublicKey(t *testing.T) {
	key := sshKey("ssh-ed25519", make([]byte, 32))
	if got, err := NormalizeSSHPublicKey("  " + key + "   andy@merrick \n"); err != nil || got != key+" andy@merrick" {
		t.Fatalf("NormalizeSSHPublicKey = %q, %v", got, err)
	}
	if got, err := NormalizeSSHPublicKey(key); err != nil || got != key {
		t.Fatalf("without a comment: %q, %v", got, err)
	}
	for name, bad := range map[string]string{
		"empty":            "",
		"options":          `command="/bin/sh" ` + key,
		"from option":      `from="10.0.0.1" ` + key,
		"two lines":        key + "\n" + key,
		"type mismatch":    "ssh-rsa " + strings.Fields(key)[1],
		"unknown type":     sshKey("ssh-dss", make([]byte, 32)),
		"not base64":       "ssh-ed25519 !!!!",
		"type only":        "ssh-ed25519",
		"control in note":  key + " a\x01b",
		"too long":         key + " " + strings.Repeat("a", MaxSSHKeyLength),
		"truncated blob":   "ssh-ed25519 " + base64.StdEncoding.EncodeToString([]byte{0, 0, 0, 99, 's'}),
		"nul":              key + "\x00",
		"carriage return":  key + "\rx",
		"comment too long": key + " " + strings.Repeat("c", 300),
	} {
		_, err := NormalizeSSHPublicKey(bad)
		var me *models.Error
		if !errors.As(err, &me) || me.Code != models.CodeInvalidRequest {
			t.Errorf("%s: %v is not invalid_request", name, err)
		}
	}
}

func TestJailFilesRender(t *testing.T) {
	key := sshKey("ssh-ed25519", make([]byte, 32))
	c, err := JailFiles{RCConf: SSHRCConf, AuthorizedKey: key + " c"}.render()
	if err != nil {
		t.Fatal(err)
	}
	want := RCConfHeader + "sshd_enable=\"YES\"\nsshd_flags=\"-o PermitRootLogin=prohibit-password\"\n"
	if string(c.rcConf) != want || string(c.authorizedKeys) != key+" c\n" {
		t.Fatalf("rendered %q / %q", c.rcConf, c.authorizedKeys)
	}
	if c, err := (JailFiles{}).render(); err != nil || c.rcConf != nil || c.authorizedKeys != nil {
		t.Fatalf("empty files: %+v, %v", c, err)
	}
	for _, bad := range []string{`a="$(id)"`, "a=\"`id`\"", `a="x" ; b="y"`, `A="x"`, `a=x`, `a="x\"`, "a=\"x\ny\"", `../a="x"`} {
		if err := (JailFiles{RCConf: []string{bad}}).Check(); err == nil {
			t.Errorf("rc.conf line %q accepted", bad)
		}
	}
	if err := (JailFiles{AuthorizedKey: "nonsense"}).Check(); err == nil {
		t.Error("a bad key was accepted")
	}
}

func jailHost(t *testing.T) (*host, *script, string) {
	t.Helper()
	mp := t.TempDir()
	run := &script{answers: map[string]any{
		"zfs get -H -p -o value mounted,mountpoint skeptest/skep/jail": "yes\n" + mp + "\n",
	}}
	h := testHost(t, run, true)
	h.conf.JailRunDir = filepath.Join(t.TempDir(), "jails")
	return h, run, mp
}

func TestJailParentRefusedBeforeAnything(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root the parent check passes; covered on hearst")
	}
	// Owned by this user, so not root's: every operation that needs the
	// parent refuses before it clones, writes, or starts anything.
	h, run, mp := jailHost(t)
	os.Chmod(mp, 0o700)
	ctx := context.Background()
	_, err := h.CreateJailRoot(ctx, "skeptest", tplA, ctrA)
	wantCode(t, err, models.CodeJailParentInsecure)
	_, err = h.StartJail(ctx, jailSpec())
	wantCode(t, err, models.CodeJailParentInsecure)
	for _, c := range run.calls {
		if strings.HasPrefix(c, "zfs clone") || strings.HasPrefix(c, "jail ") {
			t.Fatalf("ran %q although the parent was refused", c)
		}
	}
}

func TestDestroyJailRootRefusesRunning(t *testing.T) {
	h, run, _ := jailHost(t)
	run.answers["jls -j skep-"+ctrA+" jid"] = "12\n"
	_, err := h.DestroyJailRoot(context.Background(), "skeptest", ctrA)
	wantCode(t, err, models.CodeContainerNotStopped)
	err = h.ConfigureJail(context.Background(), "skeptest", ctrA, JailFiles{RCConf: SSHRCConf})
	wantCode(t, err, models.CodeContainerNotStopped)
	for _, c := range run.calls {
		if strings.HasPrefix(c, "zfs destroy") {
			t.Fatalf("destroy ran: %s", c)
		}
	}
}

func TestDestroyJailRootRefusesForeignDataset(t *testing.T) {
	ds := "skeptest/skep/jail/" + ctrA
	f := make([]string, datasetFields)
	for i := range f {
		f[i] = "-"
	}
	f[0], f[1], f[17] = ds, "filesystem", vmA
	h, run, _ := jailHost(t)
	run.answers["jls -j skep-"+ctrA+" jid"] = &CommandError{Name: "jls", ExitCode: 1, Stderr: "jls: jail \"skep-" + ctrA + "\" not found", Err: errors.New("exit status 1")}
	run.answers["zfs list -H -p -t filesystem,volume -o "+datasetColumns+" -r "+ds] = strings.Join(f, "\t") + "\n"
	if _, err := h.DestroyJailRoot(context.Background(), "skeptest", ctrA); err == nil {
		t.Fatal("a dataset naming another owner was destroyed")
	}
	for _, c := range run.calls {
		if strings.HasPrefix(c, "zfs destroy") {
			t.Fatalf("destroy ran: %s", c)
		}
	}
}

func TestRunningJails(t *testing.T) {
	h, run, _ := jailHost(t)
	run.answers["jls --libxo=json jid name path"] = `{"__version": "2", "jail-information": {"jail": [
		{"jid":12,"name":"skep-` + ctrA + `","path":"/skeptest/skep/jail/` + ctrA + `"},
		{"jid":13,"name":"skep-build-` + tplA + `","path":"/x"},
		{"jid":14,"name":"skep-notanid","path":"/y"},
		{"jid":15,"name":"www","path":"/z"}]}}`
	got, err := h.RunningJails(context.Background())
	if err != nil || len(got) != 1 || got[ctrA].JID != 12 || got[ctrA].Path != "/skeptest/skep/jail/"+ctrA {
		t.Fatalf("RunningJails = %+v, %v", got, err)
	}
}

func TestConsoleLogTail(t *testing.T) {
	h, _, _ := jailHost(t)
	if s, err := h.ConsoleLogTail(ctrA, 10); err != nil || s != "" {
		t.Fatalf("missing log: %q, %v", s, err)
	}
	if err := h.prepareJailRun(ctrA); err != nil {
		t.Fatal(err)
	}
	p, _ := h.conf.JailRunPath(ctrA, JailConsoleLog)
	os.WriteFile(p, []byte("Starting syslogd.\nStarting cron.\n"), 0o600)
	if s, err := h.ConsoleLogTail(ctrA, 15); err != nil || s != "Starting cron.\n" {
		t.Fatalf("tail = %q, %v", s, err)
	}
	if err := h.RemoveJailRun(ctrA); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(p)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the runtime directory is still there: %v", err)
	}
}

func TestPrepareJailRunRefusesOpenDirectory(t *testing.T) {
	h, _, _ := jailHost(t)
	os.MkdirAll(h.conf.JailRunDir, 0o777)
	os.Chmod(h.conf.JailRunDir, 0o777)
	if err := h.prepareJailRun(ctrA); err == nil {
		t.Fatal("a world-writable jail runtime directory was accepted")
	}
}
