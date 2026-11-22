package main

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/setup"
)

// appendSysrc is a minimal sysrc stand-in for the command tests: it
// appends name="value" (the variables in these tests are never set
// before). The full emulation is tested in internal/setup.
type appendSysrc struct{ calls []string }

var argRE = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_.]*)\+?=(.*)$`)

func (a *appendSysrc) Run(_ context.Context, c setup.Cmd) (string, error) {
	a.calls = append(a.calls, c.String())
	if c.Name == "/usr/sbin/sysrc" {
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

// A12: apply, rollback, upgrade and remove set umask 022 first.
func TestChangingCommandsSetUmask022(t *testing.T) {
	for _, args := range [][]string{{"apply", "-mgmt", "vtnet0"}, {"rollback"}, {"upgrade"}, {"remove"}} {
		root := stockRoot(t)
		var masks []int
		e := applyEnv(&appendSysrc{}, false)
		e.umask = func(m int) int { masks = append(masks, m); return 0o077 }
		runWith(t, e, append([]string{args[0], "-root", root}, args[1:]...)...)
		if len(masks) != 1 || masks[0] != 0o022 {
			t.Errorf("%s: umask calls %o", args[0], masks)
		}
	}
	var masks []int
	e := applyEnv(&appendSysrc{}, false)
	e.umask = func(m int) int { masks = append(masks, m); return 0 }
	runWith(t, e, "plan", "-root", stockRoot(t), "-mgmt", "vtnet0")
	if len(masks) != 0 {
		t.Error("plan, which changes nothing, set the umask")
	}
}

// A13: a failed administrator leaves a finished journal and names the
// command that retries it.
func TestApplyAdminFailureNamesTheRetry(t *testing.T) {
	root := stockRoot(t)
	e := applyEnv(&appendSysrc{}, true)
	e.sleep = func(time.Duration) {}
	args := []string{"apply", "-root", root, "-mgmt", "vtnet0", "-start", "-admin", "andy"}
	res := runWith(t, e, args...)
	if res.code != exitRefused || !strings.Contains(res.stderr, "creating the administrator andy failed") ||
		!strings.Contains(res.stderr, "skep-setup "+strings.Join(args, " ")) {
		t.Fatalf("%+v", res)
	}
	js, err := setup.LoadJournals(root)
	if err != nil || len(js) != 1 || !js[0].Complete {
		t.Fatalf("journals %+v %v", js, err)
	}
}

// A14: rollback says which run it undoes before it asks.
func TestRollbackNamesTheRunBeforeAsking(t *testing.T) {
	root := stockRoot(t)
	if res := runWith(t, applyEnv(&appendSysrc{}, true), "apply", "-root", root, "-mgmt", "vtnet0"); res.code != exitOK {
		t.Fatalf("%+v", res)
	}
	js, _ := setup.LoadJournals(root)
	res := runWith(t, applyEnv(&appendSysrc{}, false), "rollback", "-root", root)
	if res.code != exitPending || !strings.Contains(res.stdout, "rollback run "+js[0].ID+": apply, started ") {
		t.Fatalf("%+v", res)
	}
	if res := runWith(t, applyEnv(&appendSysrc{}, false), "rollback", "-root", root, "20200101T000000Z"); res.code != exitRefused ||
		!strings.Contains(res.stderr, "no journal 20200101T000000Z") {
		t.Fatalf("unknown id: %+v", res)
	}
}

// B: with the real runner, a -root other than / refuses what would act on
// this host whatever the root: services, accounts, the administrator, and
// upgrade.
func TestFixtureRootWithTheRealRunner(t *testing.T) {
	root := stockRoot(t)
	before := snapshot(t, root)
	e := applyEnv(setup.HostRunner{}, true)
	res := runWith(t, e, "apply", "-root", root, "-mgmt", "vtnet0", "-start", "-yes")
	if res.code != exitUsage || !strings.Contains(res.stderr, "would act on this host's services or accounts") {
		t.Fatalf("apply: %+v", res)
	}
	e.euid = 0
	if res := runWith(t, e, "upgrade", "-root", root, "-yes"); res.code != exitUsage || !strings.Contains(res.stderr, "runs only on /") {
		t.Fatalf("upgrade: %+v", res)
	}
	if snapshot(t, root) != before {
		t.Fatal("changed the root")
	}
}
