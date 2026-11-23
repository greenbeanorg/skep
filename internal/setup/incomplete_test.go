package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func wantIncomplete(t *testing.T, err error) *Incomplete {
	t.Helper()
	var inc *Incomplete
	if !errors.As(err, &inc) {
		t.Fatalf("not incomplete: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "incomplete:") {
		t.Fatalf("message: %v", err)
	}
	return inc
}

func wantNotIncomplete(t *testing.T, err error) {
	t.Helper()
	var inc *Incomplete
	if err == nil || errors.As(err, &inc) || strings.Contains(err.Error(), "incomplete:") {
		t.Fatalf("want a plain failure, got %v", err)
	}
}

func listed(items []string, sub string) bool {
	for _, s := range items {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestApplyFailsAfterEarlierChanges(t *testing.T) {
	root := stockHost(t)
	e := newApplyEnv(t, root)
	e.runner.fail["kld_list+="] = errors.New("sysrc: boom")
	j, err := e.applier().Apply(context.Background(), compute(t, newBridgeOpts(root), facts()))
	inc := wantIncomplete(t, err)
	if !listed(inc.Changed, "vmm_load") || !listed(inc.NotChanged, "kld_list") || !listed(inc.NotChanged, "agent.json") {
		t.Fatalf("%+v", inc)
	}
	if listed(inc.Changed, "kld_list") {
		t.Fatalf("the failed action is listed as changed: %+v", inc)
	}
	msg := err.Error()
	if !strings.Contains(msg, "Do not run apply again") || !strings.Contains(msg, "skep-setup rollback "+j.ID) || strings.Contains(msg, "is safe") {
		t.Fatal(msg)
	}
}

func TestApplyFirstActionFailsChangedNothing(t *testing.T) {
	root := stockHost(t)
	e := newApplyEnv(t, root)
	e.runner.fail["vmm_load"] = errors.New("sysrc: boom")
	j, err := e.applier().Apply(context.Background(), compute(t, newBridgeOpts(root), facts()))
	wantNotIncomplete(t, err)
	if !strings.Contains(err.Error(), "nothing was changed") || !strings.Contains(err.Error(), "skep-setup rollback "+j.ID) {
		t.Fatal(err)
	}
}

// A first action that wrote the wrong value did change the file.
func TestApplyFirstActionChangedItsFile(t *testing.T) {
	root := stockHost(t)
	e := newApplyEnv(t, root)
	e.runner.lie = "wrong"
	_, err := e.applier().Apply(context.Background(), compute(t, newBridgeOpts(root), facts()))
	inc := wantIncomplete(t, err)
	if len(inc.Changed) != 0 || len(inc.Uncertain) != 1 || !strings.Contains(err.Error(), "May have changed") {
		t.Fatalf("%+v", inc)
	}
}

func TestApplyOnlyAdminFails(t *testing.T) {
	root := stockHost(t)
	e := newApplyEnv(t, root)
	o := newBridgeOpts(root)
	o.Start, o.Admin = true, "andy"
	apiStarts(root, e.runner)
	e.runner.fail["admin create"] = errors.New("skep-api: boom")
	j, err := e.applier().Apply(context.Background(), compute(t, o, facts()))
	inc := wantIncomplete(t, err)
	var ae *AdminCreateError
	if !errors.As(err, &ae) || !j.Complete {
		t.Fatalf("%v complete=%v", err, j.Complete)
	}
	if !listed(inc.Changed, "vmm_load") || !listed(inc.NotChanged, "admin create") || listed(inc.Changed, "admin create") {
		t.Fatalf("%+v", inc)
	}
	if !strings.Contains(err.Error(), "Running the same command again is safe") {
		t.Fatal(err)
	}
}

func TestApplyAdminAloneFailsChangedNothing(t *testing.T) {
	root := stockHost(t)
	e := newApplyEnv(t, root)
	o := newBridgeOpts(root)
	o.Start, o.Admin = true, "andy"
	// skep_api never writes its pid file, so the administrator fails.
	if _, err := e.applier().Apply(context.Background(), compute(t, o, facts())); err == nil {
		t.Fatal("the first run's administrator should fail")
	}
	f := facts()
	f.Running = map[string]bool{"skep_agent": true, "skep_api": true}
	p := compute(t, o, f)
	if len(p.Actions()) != 1 {
		t.Fatalf("plan %v", actions(p))
	}
	e.clock = e.clock.Add(60e9)
	_, err := e.applier().Apply(context.Background(), p)
	wantNotIncomplete(t, err)
	var ae *AdminCreateError
	if !errors.As(err, &ae) {
		t.Fatal(err)
	}
}

func TestRemoveFirstStopFailsChangedNothing(t *testing.T) {
	e, _, _ := appliedHost(t)
	e.clock = e.clock.Add(60e9)
	p := computeRemove(t, RemoveOptions{Root: e.root}, facts(), e.runner)
	e.runner.fail["skep_api stop"] = errors.New("stop: boom")
	j, err := e.remover().Apply(context.Background(), p)
	wantNotIncomplete(t, err)
	if !strings.Contains(err.Error(), "skep-setup rollback "+j.ID) {
		t.Fatal(err)
	}
}

func TestRemoveFailsAfterEarlierChanges(t *testing.T) {
	e, _, _ := appliedHost(t)
	e.clock = e.clock.Add(60e9)
	p := computeRemove(t, RemoveOptions{Root: e.root}, facts(), e.runner)
	e.runner.fail["skep_agent stop"] = errors.New("stop: boom")
	j, err := e.remover().Apply(context.Background(), p)
	inc := wantIncomplete(t, err)
	if !listed(inc.Changed, "skep_api stop") || !listed(inc.NotChanged, "skep_agent stop") {
		t.Fatalf("%+v", inc)
	}
	msg := err.Error()
	if !strings.Contains(msg, "Do not run remove again") || !strings.Contains(msg, "skep-setup rollback "+j.ID) ||
		!strings.Contains(msg, "does not start services") || strings.Contains(msg, "is safe") {
		t.Fatal(msg)
	}
}

// stopGoes fails "service NAME stop" although the service did stop.
type stopGoes struct {
	*fakeRunner
	svc string
}

func (s stopGoes) Run(ctx context.Context, c Cmd) (string, error) {
	if c.Name == cmdService && len(c.Args) == 2 && c.Args[0] == s.svc && c.Args[1] == "stop" {
		s.running[s.svc] = false
		return "", errors.New("stop: reported failure")
	}
	return s.fakeRunner.Run(ctx, c)
}

func TestRemoveFailedStopThatStoppedIsUncertain(t *testing.T) {
	e, _, _ := appliedHost(t)
	e.clock = e.clock.Add(60e9)
	p := computeRemove(t, RemoveOptions{Root: e.root}, facts(), e.runner)
	a := e.remover()
	a.Runner = stopGoes{e.runner, "skep_api"}
	_, err := a.Apply(context.Background(), p)
	inc := wantIncomplete(t, err)
	if !listed(inc.Uncertain, "skep_api stop") {
		t.Fatalf("%+v", inc)
	}
}

// A stop that found the service not running changed nothing, so a later
// failure with no other change is not incomplete.
func TestRemoveNoopStopsAreNotChanges(t *testing.T) {
	e, _, _ := appliedHost(t)
	e.clock = e.clock.Add(60e9)
	e.runner.running["skep_agent"], e.runner.running["skep_api"] = false, false
	p := computeRemove(t, RemoveOptions{Root: e.root}, facts(), e.runner)
	e.runner.fail["skep_agent_enable"] = errors.New("sysrc: boom")
	_, err := e.remover().Apply(context.Background(), p)
	wantNotIncomplete(t, err)
}

func TestRollbackFailsAfterRestoring(t *testing.T) {
	root := stockHost(t)
	e := newApplyEnv(t, root)
	j, err := e.applier().Apply(context.Background(), compute(t, newBridgeOpts(root), facts()))
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(JournalDir(root, j.ID), "files", backupName(rcConfPath))
	good, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte("damaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Rollback(root, j.ID, e.clock)
	if err == nil || !listed(r.Restored, loaderConfPath) {
		t.Fatalf("%+v %v", r, err)
	}
	full := RollbackError(r, err)
	var inc *RollbackIncomplete
	if !errors.As(full, &inc) || !strings.HasPrefix(full.Error(), "incomplete:") ||
		!strings.Contains(full.Error(), "Running skep-setup rollback "+j.ID+" again is safe") {
		t.Fatalf("%v", full)
	}
	// The advice holds: with the cause fixed, the second run finishes.
	if err := os.WriteFile(backup, good, 0o600); err != nil {
		t.Fatal(err)
	}
	r2, err := Rollback(root, j.ID, e.clock)
	if err != nil || !listed(r2.Restored, rcConfPath) || listed(r2.Restored, loaderConfPath) {
		t.Fatalf("%+v %v", r2, err)
	}
}

func TestRollbackFailsBeforeChangingAnything(t *testing.T) {
	root := stockHost(t)
	e := newApplyEnv(t, root)
	j, err := e.applier().Apply(context.Background(), compute(t, newBridgeOpts(root), facts()))
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(JournalDir(root, j.ID), "files", backupName(loaderConfPath))
	if err := os.WriteFile(backup, []byte("damaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Rollback(root, j.ID, e.clock)
	if err == nil || len(r.Restored)+len(r.Removed) != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	wantNotIncomplete(t, RollbackError(r, err))
}
