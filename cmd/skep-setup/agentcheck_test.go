package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/setup"
	"github.com/greenbeanorg/skep/internal/startcheck"
)

// startFake is a host where every path is a clean root-owned directory,
// except the ones in bad.
type startFake struct{ bad map[string]startcheck.Info }

func (f startFake) Lstat(p string) (startcheck.Info, error) {
	if st, ok := f.bad[p]; ok {
		return st, nil
	}
	return startcheck.Info{UID: 0, Perm: fs.FileMode(0o755)}, nil
}
func (startFake) Readlink(string) (string, error) { return "", fs.ErrInvalid }
func (startFake) ACL(string) (string, error) {
	return "user::rwx\ngroup::r-x\nother::r-x\n", nil
}

func TestCheckReportsAgentStartCheckPassed(t *testing.T) {
	f := hostFacts()
	f.Running = map[string]bool{"skep_agent": true}
	r := runWith(t, env{prober: fakeProber{f: f}, startFS: startFake{}}, "check", "-root", stockRoot(t), "-mgmt", "vtnet0")
	for _, want := range []string{"agent: running", "agent start check: passed", "/usr/local/sbin/skep-agent", "/var/db/skep", "/var/run/skep"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("missing %q in:\n%s", want, r.stdout)
		}
	}
}

func TestCheckReportsAgentRefusalAsAFailedItem(t *testing.T) {
	bad := startFake{bad: map[string]startcheck.Info{"/usr/local/sbin": {UID: 0, Perm: 0o775}}}
	r := runWith(t, env{prober: fakeProber{f: hostFacts()}, startFS: bad}, "check", "-root", stockRoot(t), "-mgmt", "vtnet0")
	if r.code != exitPending {
		t.Fatalf("exit %d, want %d:\n%s", r.code, exitPending, r.stdout)
	}
	for _, want := range []string{"agent: not running", "REFUSED", "/usr/local/sbin is writable", "Fix: chmod go-w /usr/local/sbin"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("missing %q in:\n%s", want, r.stdout)
		}
	}
}

func TestCheckWithoutRootDoesNotClaimAnAgentState(t *testing.T) {
	r := runWith(t, env{prober: fakeProber{f: hostFacts()}, euid: 1000}, "check", "-root", stockRoot(t), "-mgmt", "vtnet0")
	if !strings.Contains(r.stdout, "not read") || strings.Contains(r.stdout, "agent: running") || strings.Contains(r.stdout, "agent: not running") {
		t.Errorf("output:\n%s", r.stdout)
	}
}

// A check that could not run is not a pass: a "not confirmed" item. A
// malformed agent.json is also a refusal of the plan, which sets the exit.
func TestCheckThatCouldNotRunIsNotConfirmed(t *testing.T) {
	root := stockRoot(t)
	p := filepath.Join(root, "usr/local/etc/skep/agent.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runWith(t, env{prober: fakeProber{f: hostFacts()}, startFS: startFake{}}, "check", "-root", root, "-mgmt", "vtnet0")
	if r.code != exitRefused {
		t.Fatalf("exit %d, want %d:\n%s\n%s", r.code, exitRefused, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "not confirmed") || strings.Contains(r.stdout, "start check: passed") {
		t.Errorf("output:\n%s", r.stdout)
	}
}

// With only the start check unable to run, the item alone makes it exit 1.
func TestReportAgentProblemIsAManualItem(t *testing.T) {
	root := stockRoot(t)
	p := filepath.Join(root, "usr/local/etc/skep/agent.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	items := reportAgent(root, hostFacts(), env{stdout: &out, startFS: startFake{}})
	if len(items) != 1 || items[0].Kind != setup.Manual || !strings.Contains(items[0].What, "not confirmed") {
		t.Fatalf("items %+v\n%s", items, out.String())
	}
}
