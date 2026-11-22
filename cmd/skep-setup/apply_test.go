package main

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/setup"
)

// appendSysrc is a minimal sysrc stand-in for the command tests: it
// appends name="value" (the variables in these tests are never set
// before). The full emulation is tested in internal/setup.
type appendSysrc struct{ calls []string }

var argRE = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_.]*)\+?=(.*)$`)

func (a *appendSysrc) Run(_ context.Context, c setup.Cmd) (string, error) {
	a.calls = append(a.calls, c.String())
	if c.Name == "sysrc" {
		m := argRE.FindStringSubmatch(c.Args[2])
		f, err := os.OpenFile(c.Args[1], os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if err != nil {
			return "", err
		}
		defer f.Close()
		_, err = f.WriteString(m[1] + `="` + m[2] + "\"\n")
		return "", err
	}
	return "", nil
}

func applyEnv(r setup.Runner, confirm bool) env {
	return env{prober: fakeProber{f: hostFacts()}, runner: r, euid: 1000,
		confirm: func(string) bool { return confirm },
		chown:   func(string, int, int) error { return nil }}
}

func TestApplyAsksFirst(t *testing.T) {
	root := stockRoot(t)
	before := snapshot(t, root)
	r := &appendSysrc{}
	res := runWith(t, applyEnv(r, false), "apply", "-root", root, "-mgmt", "vtnet0")
	if res.code != exitPending || !strings.Contains(res.stderr, "not confirmed") || len(r.calls) != 0 {
		t.Fatalf("%+v %v", res, r.calls)
	}
	if snapshot(t, root) != before {
		t.Fatal("changed the root without confirmation")
	}
}

func TestApplyRollbackCommands(t *testing.T) {
	root := stockRoot(t)
	rc0, _ := os.ReadFile(root + "/etc/rc.conf")
	r := &appendSysrc{}
	res := runWith(t, applyEnv(r, true), "apply", "-root", root, "-mgmt", "vtnet0")
	if res.code != exitOK || !strings.Contains(res.stdout, "undo with: skep-setup rollback") ||
		!strings.Contains(res.stdout, "RACCT takes effect only after a reboot") {
		t.Fatalf("%+v", res)
	}
	res = runWith(t, applyEnv(r, true), "apply", "-root", root, "-mgmt", "vtnet0", "-yes")
	if res.code != exitOK || !strings.Contains(res.stdout, "nothing to do") {
		t.Fatalf("second apply: %+v", res)
	}
	res = runWith(t, applyEnv(r, true), "rollback", "-root", root)
	if res.code != exitOK || !strings.Contains(res.stdout, "restored /etc/rc.conf") {
		t.Fatalf("rollback: %+v", res)
	}
	if rc, _ := os.ReadFile(root + "/etc/rc.conf"); string(rc) != string(rc0) {
		t.Fatal("rc.conf not restored")
	}
	if res := runWith(t, applyEnv(r, true), "rollback", "-root", root); res.code != exitRefused {
		t.Fatalf("nothing left to roll back: %+v", res)
	}
}

func TestChangingCommandsNeedRootOnTheRealHost(t *testing.T) {
	r := &appendSysrc{}
	probed := false
	e := applyEnv(r, true)
	e.prober = proberFunc(func() { probed = true })
	for _, args := range [][]string{
		{"apply", "-mgmt", "vtnet0", "-yes"},
		{"rollback", "-yes"},
	} {
		res := runWith(t, e, args...)
		if res.code != exitUsage || !strings.Contains(res.stderr, "run it as root") {
			t.Errorf("%v: %+v", args, res)
		}
	}
	if len(r.calls) != 0 || probed {
		t.Fatalf("ran %v, probed %v: the root check must come before anything is read", r.calls, probed)
	}
}

// proberFunc records that the host was probed, and fails.
type proberFunc func()

func (p proberFunc) Probe(context.Context) (setup.Facts, error) {
	p()
	return setup.Facts{}, setup.ErrNotFreeBSD
}
