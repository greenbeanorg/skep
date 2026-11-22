package main

import (
	"bytes"
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/setup"
)

type fakeProber struct {
	f   setup.Facts
	err error
}

func (p fakeProber) Probe(context.Context) (setup.Facts, error) { return p.f, p.err }

func hostFacts() setup.Facts {
	return setup.Facts{
		KernelRelease: "15.1-RELEASE-p7", UserlandRelease: "15.1-RELEASE-p7", Arch: "amd64",
		Interfaces: map[string][]netip.Prefix{"vtnet0": {netip.MustParsePrefix("192.0.2.10/24")}, "vtnet1": nil},
		Pools:      map[string]string{"tank": "ONLINE"},
	}
}

// stockRoot is a stock install with the package's account.
func stockRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"etc/rc.conf":        "hostname=\"skep1\"\nifconfig_vtnet0=\"inet 192.0.2.10/24\"\n",
		"boot/loader.conf":   "",
		"etc/passwd":         "root:*:0:0::/root:/bin/sh\nskep:*:409:409::/nonexistent:/usr/sbin/nologin\n",
		"etc/group":          "wheel:*:0:root\nskep:*:409:\n",
		"var/run/dmesg.boot": "VT-x: EPT,UG\n",
	}
	for p, c := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755)
		if err := os.WriteFile(filepath.Join(root, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Join(root, "usr/local/share/edk2-bhyve"), 0o755)
	return root
}

type result struct {
	code           int
	stdout, stderr string
}

func runWith(t *testing.T, e env, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	e.stdout, e.stderr = &out, &errb
	if e.stdin == nil {
		e.stdin = strings.NewReader("")
	}
	if e.now == nil {
		e.now = func() time.Time { return time.Date(2026, 11, 22, 16, 0, 0, 0, time.UTC) }
	}
	if e.confirm == nil {
		e.confirm = func(string) bool { return false }
	}
	code := run(context.Background(), args, e)
	return result{code, out.String(), errb.String()}
}

func TestCheckAndPlanAreReadOnly(t *testing.T) {
	root := stockRoot(t)
	before := snapshot(t, root)
	e := env{prober: fakeProber{f: hostFacts()}}
	r := runWith(t, e, "check", "-root", root, "-mgmt", "vtnet0", "-create-bridge", "bridge0", "-uplink", "vtnet1", "-pool", "tank")
	if r.code != exitPending || !strings.Contains(r.stdout, "read-only") || !strings.Contains(r.stdout, "to add") {
		t.Fatalf("check: %+v", r)
	}
	r = runWith(t, e, "plan", "-root", root, "-mgmt", "vtnet0", "-create-bridge", "bridge0", "-uplink", "vtnet1", "-pool", "tank")
	if r.code != exitOK || !strings.Contains(r.stdout, `-> sysrc kld_list+="if_epair netgraph ng_socket ng_ether ng_bpf"`) ||
		!strings.Contains(r.stdout, "$ ifconfig bridge0 create") {
		t.Fatalf("plan: %+v", r)
	}
	if after := snapshot(t, root); after != before {
		t.Fatal("check or plan changed the root")
	}
}

func TestCheckRefusalExitCode(t *testing.T) {
	f := hostFacts()
	f.KernelRelease, f.UserlandRelease = "14.3-RELEASE", "14.3-RELEASE"
	r := runWith(t, env{prober: fakeProber{f: f}}, "check", "-root", stockRoot(t), "-mgmt", "vtnet0")
	if r.code != exitRefused || !strings.Contains(r.stdout, "15.x only") {
		t.Fatalf("%+v", r)
	}
	r = runWith(t, env{prober: fakeProber{f: f}}, "plan", "-root", stockRoot(t), "-mgmt", "vtnet0")
	if r.code != exitRefused {
		t.Fatalf("%+v", r)
	}
}

func TestUsageErrors(t *testing.T) {
	e := env{prober: fakeProber{f: hostFacts()}}
	for _, args := range [][]string{
		{},
		{"frobnicate"},
		{"plan", "-root", stockRoot(t)}, // no -mgmt
		{"plan", "-root", stockRoot(t), "-mgmt", "vtnet0", "extra"}, // stray argument
		{"plan", "-root", stockRoot(t), "-mgmt", "vtnet0", "-dir-pool", "nopath"},
	} {
		if r := runWith(t, e, args...); r.code != exitUsage {
			t.Errorf("%v: exit %d (%s)", args, r.code, r.stderr)
		}
	}
	if r := runWith(t, e, "version"); r.code != 0 || !strings.HasPrefix(r.stdout, "skep-setup ") {
		t.Errorf("version: %+v", r)
	}
}

func TestProbeErrorOnLinux(t *testing.T) {
	r := runWith(t, env{prober: setup.HostProber{}}, "check", "-root", stockRoot(t), "-mgmt", "vtnet0")
	if r.code == exitOK || !strings.Contains(r.stderr, "FreeBSD") {
		t.Fatalf("%+v", r)
	}
}

// snapshot is every file under root with its content.
func snapshot(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		b.WriteString(rel + " " + info.Mode().String() + "\n")
		if info.Mode().IsRegular() {
			c, _ := os.ReadFile(p)
			b.Write(c)
		}
		return nil
	})
	return b.String()
}

// Finding 1 of PR #52: the plan asks skep-api only as root, only on the
// real host, and never through the real runner for a fixture root.
func TestAdminCheckerChoice(t *testing.T) {
	real := env{runner: setup.HostRunner{}, euid: 0}
	if _, ok := real.adminChecker("/").(setup.SkepAPIAdmins); !ok {
		t.Error("root on /: does not ask skep-api")
	}
	for name, c := range map[string]setup.AdminChecker{
		"fixture root": real.adminChecker("/tmp/fixture"),
		"not root":     env{runner: setup.HostRunner{}, euid: 1001}.adminChecker("/"),
	} {
		if _, ok := c.(setup.NoAdminChecker); !ok {
			t.Errorf("%s: %T, want NoAdminChecker", name, c)
		}
	}
	if _, ok := (env{runner: nil, euid: 1001}).adminChecker("/tmp/fixture").(setup.SkepAPIAdmins); !ok {
		t.Error("a test runner is not asked")
	}
}
