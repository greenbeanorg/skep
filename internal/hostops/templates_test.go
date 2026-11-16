package hostops

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

const tplA = "3f2a9c1e-7b4d-4e8f-9a01-23456789abcd"

// templateHost is a host whose <root>/templates is mounted at a temporary
// directory, with the template tplA mounted under it.
func templateHost(t *testing.T) (*host, *script, string) {
	t.Helper()
	mp := t.TempDir()
	run := &script{answers: map[string]any{
		"zfs get -H -p -o value mounted,mountpoint skeptest/skep/templates":         "yes\n" + mp + "\n",
		"zfs get -H -p -o value mounted,mountpoint skeptest/skep/templates/" + tplA: "yes\n" + filepath.Join(mp, tplA) + "\n",
	}}
	if err := os.Mkdir(filepath.Join(mp, tplA), 0o755); err != nil {
		t.Fatal(err)
	}
	return testHost(t, run, true), run, mp
}

func TestTemplateNames(t *testing.T) {
	c := testConf(t)
	ds, err := c.TemplateDataset("skeptest", tplA)
	if err != nil || ds != "skeptest/skep/templates/"+tplA {
		t.Fatalf("TemplateDataset = %q, %v", ds, err)
	}
	if _, err := c.TemplateDataset("skeptest", "../etc"); err == nil {
		t.Fatal("an invalid template ID was accepted")
	}
	if _, err := c.TemplateDataset("tank", tplA); err == nil {
		t.Fatal("a pool off the allow-list was accepted")
	}
	if n, err := BuildJailName(tplA); err != nil || n != "skep-build-"+tplA {
		t.Fatalf("BuildJailName = %q, %v", n, err)
	}
}

func TestBuildJailArgs(t *testing.T) {
	args := buildJailArgs("skep-build-"+tplA, "/skeptest/skep/templates/"+tplA)
	joined := " " + strings.Join(args, " ") + " "
	for _, want := range []string{
		" -c ", " persist ", " ip4=disable ", " ip6=disable ", " enforce_statfs=2 ", " children.max=0 ",
		" securelevel=2 ", " exec.clean ", " allow.suser=0 ", " allow.reserved_ports=0 ",
		" allow.unprivileged_proc_debug=0 ", " allow.mount=0 ", " allow.raw_sockets=0 ",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("jail -c arguments lack %q", strings.TrimSpace(want))
		}
	}
	for _, never := range []string{"vnet", "mount.devfs", "exec.start", "exec.stop", "command=", "exec.prestart", "=1 "} {
		if strings.Contains(joined, never) {
			t.Errorf("jail -c arguments contain %q", never)
		}
	}
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "allow.") && !strings.HasSuffix(a, "=0") {
			t.Errorf("%s is not off", a)
		}
	}
}

func loadJailParams(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/jls-build-jail-15.1.json")
	if err != nil {
		t.Fatal(err)
	}
	h := testHost(t, &script{answers: map[string]any{"jls --libxo=json -n -j x": string(raw)}}, true)
	params, err := h.jailParams(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	return params
}

// TestCheckBuildJail uses jls output recorded on hearst (15.1-RELEASE-p6)
// for a jail created with the build jail's parameters.
func TestCheckBuildJail(t *testing.T) {
	params := loadJailParams(t)
	name, root := params["name"].(string), params["path"].(string)
	if err := checkBuildJail(params, name, root); err != nil {
		t.Fatalf("the recorded build jail is refused: %v", err)
	}
	for k, v := range map[string]any{
		"allow.reserved_ports": true, "allow.suser": true, "allow.vmm": true, "securelevel": "1",
		"ip4": "inherit", "vnet": "new", "children.max": "1", "enforce_statfs": "1",
	} {
		bad := map[string]any{}
		for kk, vv := range params {
			bad[kk] = vv
		}
		bad[k] = v
		if err := checkBuildJail(bad, name, root); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%v was accepted (err %v)", k, v, err)
		}
	}
	if err := checkBuildJail(params, name, "/elsewhere"); err == nil {
		t.Error("a jail on another path was accepted")
	}
	noAllow := map[string]any{}
	for k, v := range params {
		if !strings.HasPrefix(k, "allow.") {
			noAllow[k] = v
		}
	}
	if err := checkBuildJail(noAllow, name, root); err == nil {
		t.Error("a report without allow.* parameters was accepted")
	}
}

func TestStartBuildJailChecksParameters(t *testing.T) {
	h, run, mp := templateHost(t)
	if err := os.Chmod(mp, 0o700); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		// The parent must be root's; as a normal user the check refuses
		// before jail -c runs, which is itself what this asserts.
		err := h.StartBuildJail(context.Background(), "skeptest", tplA)
		wantCode(t, err, models.CodeJailParentInsecure)
		for _, c := range run.calls {
			if strings.HasPrefix(c, "jail ") {
				t.Fatalf("jail ran although the parent was refused: %s", c)
			}
		}
		return
	}
	t.Skip("covered by the FreeBSD integration test when run as root")
}

func TestBuildJailExec(t *testing.T) {
	h, run, mp := templateHost(t)
	ran, err := h.BuildJailExec(context.Background(), "skeptest", tplA, BuildCertctl)
	if err != nil || !ran {
		t.Fatalf("BuildJailExec = %v, %v", ran, err)
	}
	want := "jexec -l skep-build-" + tplA + " /usr/sbin/certctl rehash"
	if got := run.calls[len(run.calls)-1]; got != want {
		t.Fatalf("ran %q, want %q", got, want)
	}
	for _, c := range run.calls {
		if strings.Contains(c, " -U") || strings.Contains(c, " -u") {
			t.Fatalf("jexec was given a user: %s", c)
		}
	}
	if got := run.envs[len(run.envs)-1]; !reflect.DeepEqual(got, []string{"PATH=/sbin:/bin:/usr/sbin:/usr/bin", "LC_ALL=C"}) {
		t.Fatalf("jexec environment = %v", got)
	}
	// services_mkdb reads /etc/services, not the package script's
	// "${PKG_ROOTDIR}/" (the step 6 probe).
	if _, err := h.BuildJailExec(context.Background(), "skeptest", tplA, BuildServicesMkdb); err != nil {
		t.Fatal(err)
	}
	if got := run.calls[len(run.calls)-1]; !strings.HasSuffix(got, "/usr/sbin/services_mkdb -l -q -o /var/db/services.db /etc/services") {
		t.Fatalf("services_mkdb ran as %q", got)
	}
	// The OpenSSL manual pages are indexed only when they exist.
	n := len(run.calls)
	if ran, err := h.BuildJailExec(context.Background(), "skeptest", tplA, BuildMakewhatisOpenSSL); err != nil || ran {
		t.Fatalf("makewhatis without the directory = %v, %v", ran, err)
	}
	for _, c := range run.calls[n:] {
		if strings.HasPrefix(c, "jexec") {
			t.Fatalf("a command ran for an absent directory: %s", c)
		}
	}
	if err := os.MkdirAll(filepath.Join(mp, tplA, "usr/share/openssl/man"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ran, err := h.BuildJailExec(context.Background(), "skeptest", tplA, BuildMakewhatisOpenSSL); err != nil || !ran {
		t.Fatalf("makewhatis with the directory = %v, %v", ran, err)
	}
	if _, err := h.BuildJailExec(context.Background(), "skeptest", tplA, BuildCommand(99)); err == nil {
		t.Fatal("an unknown build command ran")
	}
}

func TestBuildCommandOrder(t *testing.T) {
	var got []string
	for _, c := range BuildCommands {
		got = append(got, buildCommandSpecs[c].argv[0])
	}
	want := []string{"/usr/sbin/certctl", "/usr/sbin/pwd_mkdb", "/usr/sbin/services_mkdb", "/usr/bin/cap_mkdb", "/usr/bin/makewhatis", "/usr/bin/makewhatis"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v", got)
	}
}

func TestRemoveBuildJail(t *testing.T) {
	name := "skep-build-" + tplA
	gone := &CommandError{Name: "jls", ExitCode: 1, Stderr: `jls: jail "` + name + `" not found`}
	run := &script{answers: map[string]any{"jls -j " + name + " jid": gone}}
	h := testHost(t, run, true)
	if err := h.RemoveBuildJail(context.Background(), tplA); err != nil {
		t.Fatal(err)
	}
	if len(run.calls) != 1 {
		t.Fatalf("a missing jail was removed: %v", run.calls)
	}
	// One that stays after jail -r is an error.
	run = &script{answers: map[string]any{"jls -j " + name + " jid": "7\n"}}
	h = testHost(t, run, true)
	if err := h.RemoveBuildJail(context.Background(), tplA); err == nil || !strings.Contains(err.Error(), "still there") {
		t.Fatalf("a jail that stayed = %v", err)
	}
	if run.calls[1] != "jail -r "+name {
		t.Fatalf("calls = %v", run.calls)
	}
}

func TestBuildJails(t *testing.T) {
	run := &script{answers: map[string]any{"jls --libxo=json name": `{"__version": "2", "jail-information": {"jail": [` +
		`{"name":"skep-build-` + tplA + `"},{"name":"skep-` + vmA + `"},{"name":"skep-build-nope"},{"name":"web"}]}}`}}
	ids, err := testHost(t, run, true).BuildJails(context.Background())
	if err != nil || !reflect.DeepEqual(ids, []string{tplA}) {
		t.Fatalf("BuildJails = %v, %v", ids, err)
	}
}

func TestPkgInstall(t *testing.T) {
	h, run, mp := templateHost(t)
	root := filepath.Join(mp, tplA)
	work := filepath.Join(mp, ".work-"+tplA)
	if err := h.PkgInstall(context.Background(), "skeptest", tplA, []string{"FreeBSD-set-minimal"}); err != nil {
		t.Fatal(err)
	}
	want := "pkg --rootdir " + root + " -o PKG_CACHEDIR=" + work + "/cache -o PKG_DBDIR=" + root +
		"/var/db/pkg -o RUN_SCRIPTS=false -o PKG_TRIGGERS_ENABLE=false install -y FreeBSD-set-minimal"
	if got := run.calls[len(run.calls)-1]; got != want {
		t.Fatalf("pkg ran as\n%s\nwant\n%s", got, want)
	}
	wantEnv := []string{"PATH=/sbin:/bin:/usr/sbin:/usr/bin", "ASSUME_ALWAYS_YES=yes", "TMPDIR=" + work + "/tmp"}
	if got := run.envs[0]; !reflect.DeepEqual(got, wantEnv) {
		t.Fatalf("pkg environment = %v, want %v", got, wantEnv)
	}
	for _, bad := range [][]string{nil, {"pkg"}, {"FreeBSD-set-minimal; rm -rf /"}, {"FreeBSD-"}, {"-o"}, {"FreeBSD-Foo"}} {
		if err := h.PkgInstall(context.Background(), "skeptest", tplA, bad); err == nil {
			t.Errorf("packages %q were accepted", bad)
		}
	}
}

func TestPkgScriptsCommand(t *testing.T) {
	h, run, mp := templateHost(t)
	root := filepath.Join(mp, tplA)
	call := "pkg --rootdir " + root + " -o PKG_DBDIR=" + root + "/var/db/pkg info --raw --raw-format json-compact -a"
	run.answers[call] = `[{"name":"FreeBSD-utilities","version":"15.1p4","scripts":{"post-install":"cap_mkdb -l ${PKG_ROOTDIR}/etc/login.conf"}}]`
	got, err := h.PkgScripts(context.Background(), "skeptest", tplA)
	if err != nil || len(got) != 1 || got[0].Scripts["post-install"] != "cap_mkdb -l ${PKG_ROOTDIR}/etc/login.conf" {
		t.Fatalf("PkgScripts = %+v, %v", got, err)
	}
}

func TestParsePkgRaw(t *testing.T) {
	arr := `[{"name":"A","version":"1","scripts":{"post-install":"x"}},{"name":"B","version":"2","lua_scripts":{"post-install":["y","z"]}}]`
	got, err := parsePkgRaw([]byte(arr))
	if err != nil || len(got) != 2 || got[1].LuaScripts["post-install"][1] != "z" {
		t.Fatalf("array: %+v, %v", got, err)
	}
	lines := `{"name":"A","version":"1"}` + "\n" + `{"name":"B","version":"2"}` + "\n"
	if got, err := parsePkgRaw([]byte(lines)); err != nil || len(got) != 2 {
		t.Fatalf("lines: %+v, %v", got, err)
	}
	for _, bad := range []string{"", "garbage", `[{"version":"1"}]`, `[{"name":"A"}`} {
		if _, err := parsePkgRaw([]byte(bad)); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestTemplateRelease(t *testing.T) {
	h, _, mp := templateHost(t)
	bin := filepath.Join(mp, tplA, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n# comment\nUSERLAND_VERSION=\"15.1-RELEASE-p7\"\n: ${ROOT:=}\n"
	if err := os.WriteFile(filepath.Join(bin, "freebsd-version"), []byte(script), 0o555); err != nil {
		t.Fatal(err)
	}
	if rel, err := h.TemplateRelease(context.Background(), "skeptest", tplA); err != nil || rel != "15.1-RELEASE-p7" {
		t.Fatalf("TemplateRelease = %q, %v", rel, err)
	}
	// A link in place of the script is not followed.
	os.Remove(filepath.Join(bin, "freebsd-version"))
	if err := os.Symlink("/bin/freebsd-version", filepath.Join(bin, "freebsd-version")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.TemplateRelease(context.Background(), "skeptest", tplA); err == nil {
		t.Fatal("a symbolic link was read")
	}
	for _, bad := range []string{"USERLAND_VERSION=\"15.1; rm\"\n", "no version here\n"} {
		if _, err := parseUserland([]byte(bad)); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestHostRelease(t *testing.T) {
	run := &script{answers: map[string]any{"freebsd-version -k": "15.1-RELEASE-p6\n"}}
	if rel, err := testHost(t, run, true).HostRelease(context.Background()); err != nil || rel != "15.1-RELEASE-p6" {
		t.Fatalf("HostRelease = %q, %v", rel, err)
	}
	run.answers["freebsd-version -k"] = "nonsense\n"
	if _, err := testHost(t, run, true).HostRelease(context.Background()); err == nil {
		t.Fatal("nonsense was accepted")
	}
}

func TestTriggerFiles(t *testing.T) {
	h, _, mp := templateHost(t)
	root := filepath.Join(mp, tplA)
	if files, err := h.TriggerFiles(context.Background(), "skeptest", tplA); err != nil || len(files) != 0 {
		t.Fatalf("no trigger directories: %v, %v", files, err)
	}
	dir := filepath.Join(root, "usr/share/pkg/triggers")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "mandoc.ucl"), []byte("trigger"), 0o444)
	os.Symlink("/etc/passwd", filepath.Join(dir, "evil.ucl"))
	files, err := h.TriggerFiles(context.Background(), "skeptest", tplA)
	if err != nil || len(files) != 2 {
		t.Fatalf("TriggerFiles = %+v, %v", files, err)
	}
	for _, f := range files {
		switch f.Path {
		case "usr/share/pkg/triggers/mandoc.ucl":
			if !f.Regular || string(f.Content) != "trigger" {
				t.Errorf("mandoc.ucl = %+v", f)
			}
		case "usr/share/pkg/triggers/evil.ucl":
			if f.Regular || len(f.Content) != 0 {
				t.Errorf("the link was read: %+v", f)
			}
		default:
			t.Errorf("unexpected %s", f.Path)
		}
	}
	// A trigger directory that is a link is refused, not followed.
	os.MkdirAll(filepath.Join(root, "usr/local/share"), 0o755)
	os.Symlink("/etc", filepath.Join(root, "usr/local/share/pkg"))
	if _, err := h.TriggerFiles(context.Background(), "skeptest", tplA); err == nil {
		t.Fatal("a linked trigger directory was followed")
	}
}

func TestTemplateTreeChecks(t *testing.T) {
	h, _, mp := templateHost(t)
	root := filepath.Join(mp, tplA)
	ctx := context.Background()
	if l, err := h.DeferredTriggers(ctx, "skeptest", tplA); err != nil || len(l) != 0 {
		t.Fatalf("absent triggers dir: %v, %v", l, err)
	}
	os.MkdirAll(filepath.Join(root, "var/db/pkg/triggers"), 0o755)
	os.MkdirAll(filepath.Join(root, "var/db/pkg/repos/FreeBSD-ports"), 0o755)
	os.WriteFile(filepath.Join(root, "var/db/pkg/repos/FreeBSD-ports/db"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "var/db/pkg/local.sqlite"), []byte("x"), 0o644)
	if l, err := h.DeferredTriggers(ctx, "skeptest", tplA); err != nil || len(l) != 0 {
		t.Fatalf("empty triggers dir: %v, %v", l, err)
	}
	os.WriteFile(filepath.Join(root, "var/db/pkg/triggers/oR3hHb5PjH"), []byte("x"), 0o644)
	if l, err := h.DeferredTriggers(ctx, "skeptest", tplA); err != nil || !reflect.DeepEqual(l, []string{"var/db/pkg/triggers/oR3hHb5PjH"}) {
		t.Fatalf("a deferred trigger: %v, %v", l, err)
	}
	if err := h.RemoveCatalogues(ctx, "skeptest", tplA); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "var/db/pkg/repos")); !os.IsNotExist(err) {
		t.Fatalf("repos is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "var/db/pkg/local.sqlite")); err != nil {
		t.Fatalf("the package database was removed: %v", err)
	}
	if l, err := h.CacheEntries(ctx, "skeptest", tplA); err != nil || len(l) != 0 {
		t.Fatalf("absent cache: %v, %v", l, err)
	}
	os.MkdirAll(filepath.Join(root, "var/cache/pkg"), 0o755)
	os.WriteFile(filepath.Join(root, "var/cache/pkg/FreeBSD-runtime.pkg"), []byte("x"), 0o644)
	if l, err := h.CacheEntries(ctx, "skeptest", tplA); err != nil || len(l) != 1 {
		t.Fatalf("a cached package: %v, %v", l, err)
	}
	// A var/db/pkg that is a link out of the root is refused.
	os.RemoveAll(filepath.Join(root, "var/db/pkg"))
	os.Symlink("/var/db/pkg", filepath.Join(root, "var/db/pkg"))
	if err := h.RemoveCatalogues(ctx, "skeptest", tplA); err == nil {
		t.Fatal("a linked var/db/pkg was followed")
	}
}

func TestTemplateMustBeMountedInPlace(t *testing.T) {
	h, run, mp := templateHost(t)
	run.answers["zfs get -H -p -o value mounted,mountpoint skeptest/skep/templates/"+tplA] = "yes\n/somewhere/else\n"
	if _, err := h.TriggerFiles(context.Background(), "skeptest", tplA); err == nil {
		t.Fatal("a template mounted elsewhere was used")
	}
	run.answers["zfs get -H -p -o value mounted,mountpoint skeptest/skep/templates/"+tplA] = "no\n" + filepath.Join(mp, tplA) + "\n"
	if _, err := h.TriggerFiles(context.Background(), "skeptest", tplA); err == nil {
		t.Fatal("an unmounted template was used")
	}
}

func TestCheckPrivateDir(t *testing.T) {
	dir := t.TempDir()
	wantCode(t, checkPrivateDir(filepath.Join(dir, "missing")), models.CodeJailParentInsecure)
	link := filepath.Join(dir, "link")
	os.Symlink(dir, link)
	wantCode(t, checkPrivateDir(link), models.CodeJailParentInsecure)
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o700)
	if os.Geteuid() != 0 {
		// Owned by this user, not root.
		wantCode(t, checkPrivateDir(sub), models.CodeJailParentInsecure)
		return
	}
	if err := checkPrivateDir(sub); err != nil {
		t.Fatal(err)
	}
	os.Chmod(sub, 0o755)
	wantCode(t, checkPrivateDir(sub), models.CodeJailParentInsecure)
}

func TestHostFiles(t *testing.T) {
	dir := t.TempDir()
	etc := filepath.Join(dir, "etc")
	os.MkdirAll(filepath.Join(etc, "ssl"), 0o755)
	os.WriteFile(filepath.Join(etc, "rc.conf"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(etc, "ssl", "openssl.cnf"), []byte("b"), 0o644)
	os.Symlink("../x", filepath.Join(etc, "localtime"))
	roots := []string{etc, filepath.Join(dir, "missing")}
	ctx := context.Background()
	before, err := recordFiles(ctx, roots)
	if err != nil || len(before) != 4 {
		t.Fatalf("recordFiles = %v, %v", before, err)
	}
	same, _ := recordFiles(ctx, roots)
	if c := before.Changed(same); len(c) != 0 {
		t.Fatalf("no change reported %v", c)
	}
	// Content, a link's target, mode, mtime, a new file, and a removed one.
	os.WriteFile(filepath.Join(etc, "rc.conf"), []byte("c"), 0o644)
	os.Remove(filepath.Join(etc, "localtime"))
	os.Symlink("../y", filepath.Join(etc, "localtime"))
	os.Chmod(filepath.Join(etc, "ssl", "openssl.cnf"), 0o600)
	os.WriteFile(filepath.Join(etc, "new"), nil, 0o644)
	after, _ := recordFiles(ctx, roots)
	want := []string{filepath.Join(etc, "localtime"), filepath.Join(etc, "new"), filepath.Join(etc, "rc.conf"), filepath.Join(etc, "ssl", "openssl.cnf")}
	if c := before.Changed(after); !reflect.DeepEqual(c, want) {
		t.Fatalf("Changed = %v, want %v", c, want)
	}
	// Same content, new mtime.
	again, _ := recordFiles(ctx, roots)
	os.Chtimes(filepath.Join(etc, "new"), time.Now(), time.Now().Add(time.Hour))
	later, _ := recordFiles(ctx, roots)
	if c := again.Changed(later); !reflect.DeepEqual(c, []string{filepath.Join(etc, "new")}) {
		t.Fatalf("mtime change: %v", c)
	}
	// The default roots are the decision's.
	if !reflect.DeepEqual(HostFileRoots, []string{"/etc", "/var/db/pkg", "/usr/share/certs"}) {
		t.Fatalf("HostFileRoots = %v", HostFileRoots)
	}
}

func TestManifestSHA256(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	m := "base-dbg.txz\t" + strings.Repeat("cd", 32) + "\t10\tbase_dbg\t\"Base\"\toff\nbase.txz\t" + sum + "\t31000\tbase\t\"Base system (MANDATORY)\"\ton\n"
	if got, err := manifestSHA256([]byte(m), "base.txz"); err != nil || got != sum {
		t.Fatalf("manifestSHA256 = %q, %v", got, err)
	}
	if _, err := manifestSHA256([]byte("base.txz\tnothex\n"), "base.txz"); err == nil {
		t.Fatal("a bad sum was accepted")
	}
	if _, err := manifestSHA256([]byte(m), "lib32.txz"); err == nil {
		t.Fatal("a missing entry was accepted")
	}
}

func TestRemoveWorkArea(t *testing.T) {
	h, _, mp := templateHost(t)
	work := filepath.Join(mp, ".work-"+tplA)
	os.MkdirAll(filepath.Join(work, "cache"), 0o700)
	os.WriteFile(filepath.Join(work, "cache", "x.pkg"), []byte("x"), 0o600)
	if err := h.RemoveWorkArea(context.Background(), "skeptest", tplA); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(work); !os.IsNotExist(err) {
		t.Fatalf("work area still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mp, tplA)); err != nil {
		t.Fatalf("the template root was removed: %v", err)
	}
	if err := h.RemoveWorkArea(context.Background(), "skeptest", tplA); err != nil {
		t.Fatalf("a missing work area: %v", err)
	}
}

func TestDestroyTemplateRefusesForeignDataset(t *testing.T) {
	ds := "skeptest/skep/templates/" + tplA
	list := func(prop string) string {
		f := make([]string, datasetFields)
		for i := range f {
			f[i] = "-"
		}
		f[0], f[1], f[18] = ds, "filesystem", prop
		return strings.Join(f, "\t") + "\n"
	}
	run := &script{answers: map[string]any{
		"zfs list -H -p -t filesystem,volume -o " + datasetColumns + " -r " + ds: list(vmA),
	}}
	if _, err := testHost(t, run, true).DestroyTemplate(context.Background(), "skeptest", tplA); err == nil {
		t.Fatal("a dataset naming another owner was destroyed")
	}
	for _, c := range run.calls {
		if strings.HasPrefix(c, "zfs destroy") {
			t.Fatalf("destroy ran: %s", c)
		}
	}
}
