package main

import (
	"context"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/setup"
)

// versionRunner answers -version with another build and records the rest.
type versionRunner struct{ calls []string }

func (v *versionRunner) Run(_ context.Context, c setup.Cmd) (string, error) {
	v.calls = append(v.calls, c.String())
	if len(c.Args) == 1 && c.Args[0] == "-version" {
		return "skep-agent 9.9.9 (commit zzz, built x, go freebsd/amd64)\n", nil
	}
	return "", nil
}

func TestUpgradeCommand(t *testing.T) {
	r := &versionRunner{}
	e := env{prober: fakeProber{f: hostFacts()}, runner: r, euid: 1000, confirm: func(string) bool { return true }}
	if res := runWith(t, e, "upgrade", "-yes"); res.code != exitUsage || !strings.Contains(res.stderr, "run it as root") || len(r.calls) != 0 {
		t.Fatalf("non-root on /: %+v %v", res, r.calls)
	}
	root := t.TempDir()
	e.confirm = func(string) bool { return false }
	if res := runWith(t, e, "upgrade", "-root", root); res.code != exitPending || len(r.calls) != 0 {
		t.Fatalf("unconfirmed: %+v %v", res, r.calls)
	}
	res := runWith(t, e, "upgrade", "-root", root, "-yes")
	if res.code != exitRefused || !strings.Contains(res.stderr, "not upgraded as one") {
		t.Fatalf("mismatch: %+v", res)
	}
	for _, c := range r.calls {
		if strings.HasPrefix(c, "service") || strings.Contains(c, "db backup") {
			t.Fatalf("ran %q while refusing", c)
		}
	}
}

func TestUpgradeWaitFlag(t *testing.T) {
	r := &versionRunner{}
	e := env{prober: fakeProber{f: hostFacts()}, runner: r, euid: 0, confirm: func(string) bool { return true }}
	root := t.TempDir()
	if res := runWith(t, e, "upgrade", "-root", root, "-yes", "-wait", "-1s"); res.code != exitUsage || !strings.Contains(res.stderr, "must not be negative") || len(r.calls) != 0 {
		t.Fatalf("negative -wait: %+v %v", res, r.calls)
	}
	if res := runWith(t, e, "upgrade", "-root", root, "-yes", "-wait", "soon"); res.code != exitUsage || len(r.calls) != 0 {
		t.Fatalf("bad -wait: %+v %v", res, r.calls)
	}
	// 0 and a duration are accepted (the run then refuses on the build).
	for _, w := range []string{"0", "90s"} {
		if res := runWith(t, e, "upgrade", "-root", root, "-yes", "-wait", w); res.code != exitRefused || !strings.Contains(res.stderr, "not upgraded as one") {
			t.Fatalf("-wait %s: %+v", w, res)
		}
	}
}
