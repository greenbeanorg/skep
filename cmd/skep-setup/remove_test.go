package main

import (
	"strings"
	"testing"
)

func TestRemoveCommand(t *testing.T) {
	root := stockRoot(t)
	r := &appendSysrc{}
	before := snapshot(t, root)
	res := runWith(t, applyEnv(r, true), "remove", "-plan", "-root", root)
	if res.code != exitOK || !strings.Contains(res.stdout, "read-only") || !strings.Contains(res.stdout, "service skep_api stop") ||
		!strings.Contains(res.stdout, "$ pkg delete skep") {
		t.Fatalf("%+v", res)
	}
	if snapshot(t, root) != before || len(r.calls) != 0 {
		t.Fatalf("remove -plan changed something: %v", r.calls)
	}
	// -purge-data always asks, even with -yes.
	asked := 0
	e := applyEnv(r, false)
	e.confirm = func(string) bool { asked++; return false }
	res = runWith(t, e, "remove", "-root", root, "-purge-data", "-yes")
	if res.code != exitPending || asked != 1 || snapshot(t, root) != before {
		t.Fatalf("purge without confirmation: %+v asked %d", res, asked)
	}
	res = runWith(t, applyEnv(r, true), "remove", "-root", root, "-yes")
	if res.code != exitOK || !strings.Contains(res.stdout, "pkg delete skep") {
		t.Fatalf("%+v", res)
	}
	// On the real host, changing needs root, and is checked before anything is read.
	res = runWith(t, applyEnv(r, true), "remove", "-yes")
	if res.code != exitUsage || !strings.Contains(res.stderr, "run it as root") {
		t.Fatalf("%+v", res)
	}
}
