package setup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if !strings.Contains(err.Error(), "the host's configuration was not changed") || !strings.Contains(err.Error(), "skep-setup rollback "+j.ID) {
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
	f.Running = map[string]bool{"skep_agent": true, "skep_cluster": true, "skep_api": true}
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

// A failed stop may have delivered its signal, so it is uncertain even
// though the service still runs, and no status is read after it.
func TestRemoveFailedStopStillRunningIsUncertain(t *testing.T) {
	e, _, _ := appliedHost(t)
	e.clock = e.clock.Add(60e9)
	p := computeRemove(t, RemoveOptions{Root: e.root}, facts(), e.runner)
	e.runner.fail["skep_api stop"] = errors.New("stop: boom")
	j, err := e.remover().Apply(context.Background(), p)
	inc := wantIncomplete(t, err)
	if !listed(inc.Uncertain, "skep_api stop") || !strings.Contains(err.Error(), "skep-setup rollback "+j.ID) {
		t.Fatalf("%+v", inc)
	}
	if last := e.runner.calls[len(e.runner.calls)-1]; !strings.Contains(last, "skep_api stop") {
		t.Fatalf("a command ran after the failed stop: %s", last)
	}
}

func TestRemoveFailsAfterEarlierChanges(t *testing.T) {
	e, _, _ := appliedHost(t)
	e.clock = e.clock.Add(60e9)
	p := computeRemove(t, RemoveOptions{Root: e.root}, facts(), e.runner)
	e.runner.fail["skep_agent stop"] = errors.New("stop: boom")
	j, err := e.remover().Apply(context.Background(), p)
	inc := wantIncomplete(t, err)
	if !listed(inc.Changed, "skep_api stop") || !listed(inc.Uncertain, "skep_agent stop") {
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

// writeNew must not touch a path that exists, whatever its content, so a
// failure on one is no change.
func TestFailedWriteOnExistingFileChangedNothing(t *testing.T) {
	root := stockHost(t)
	a := newApplyEnv(t, root).applier()
	const rel = "/etc/skep-existing"
	full := filepath.Join(root, rel)
	if err := os.WriteFile(full, []byte("theirs"), 0o640); err != nil {
		t.Fatal(err)
	}
	act := Action{Type: ActWrite, File: rel, Content: []byte("ours"), Mode: 0o600, Owner: "root", Group: "wheel"}
	before := a.snapshot(act, "")
	if err := a.writeNew(act); err == nil {
		t.Fatal("writeNew replaced an existing file")
	}
	b, err := os.ReadFile(full)
	st, serr := os.Stat(full)
	if err != nil || serr != nil || string(b) != "theirs" || st.Mode().Perm() != 0o640 {
		t.Fatalf("existing file changed: %q %v %v %v", b, st.Mode(), err, serr)
	}
	if got := a.failedEffect(act, "", before); got != effectNone {
		t.Fatalf("effect %v", got)
	}
}

// mkdir must not set a mode or owner on a directory that exists.
func TestFailedMkdirOnExistingDirChangedNothing(t *testing.T) {
	root := stockHost(t)
	a := newApplyEnv(t, root).applier()
	const rel = "/etc/skep-existing-dir"
	full := filepath.Join(root, rel)
	if err := os.Mkdir(full, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(full, 0o750); err != nil {
		t.Fatal(err)
	}
	act := Action{Type: ActMkdir, File: rel, Mode: 0o700, Owner: "root", Group: "wheel"}
	before := a.snapshot(act, "")
	if err := a.mkdir(act); err == nil {
		t.Fatal("mkdir succeeded on an existing directory")
	}
	if st, err := os.Stat(full); err != nil || st.Mode().Perm() != 0o750 {
		t.Fatalf("existing directory changed: %v %v", st.Mode(), err)
	}
	if got := a.failedEffect(act, "", before); got != effectNone {
		t.Fatalf("effect %v", got)
	}
}

func TestMarkInterrupted(t *testing.T) {
	var killed *interruptedError
	bg := context.Background()
	if err := markInterrupted(bg, exec.Command("/bin/sh", "-c", "exit 3").Run()); errors.As(err, &killed) {
		t.Fatalf("a clean non-zero exit counted as killed: %v", err)
	}
	if err := markInterrupted(bg, exec.Command("/bin/sh", "-c", "kill -9 $$").Run()); !errors.As(err, &killed) {
		t.Fatalf("a killed command not marked: %v", err)
	}
	ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	if err := markInterrupted(ctx, exec.CommandContext(ctx, "/bin/sleep", "5").Run()); !errors.As(err, &killed) {
		t.Fatalf("a timed-out command not marked: %v", err)
	}
	if markInterrupted(bg, nil) != nil {
		t.Fatal("nil error wrapped")
	}
}

// adminOnlyPlan leaves a host where the administrator is the only action.
func adminOnlyPlan(t *testing.T, e *applyEnv, root string) Plan {
	t.Helper()
	o := newBridgeOpts(root)
	o.Start, o.Admin = true, "andy"
	// skep_api never writes its pid file, so the first administrator fails.
	if _, err := e.applier().Apply(context.Background(), compute(t, o, facts())); err == nil {
		t.Fatal("the first run's administrator should fail")
	}
	f := facts()
	f.Running = map[string]bool{"skep_agent": true, "skep_cluster": true, "skep_api": true}
	p := compute(t, o, f)
	if len(p.Actions()) != 1 {
		t.Fatalf("plan %v", actions(p))
	}
	// Now skep_api is up, so the next attempt reaches the command.
	for path, data := range map[string]string{apiPID: "1234\n", "/var/db/skep/api/api.db": ""} {
		full := hostPath(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e.clock = e.clock.Add(60e9)
	return p
}

func TestApplyAdminAloneKilledIsUncertain(t *testing.T) {
	root := stockHost(t)
	e := newApplyEnv(t, root)
	p := adminOnlyPlan(t, e, root)
	e.runner.fail["admin create"] = exec.Command("/bin/sh", "-c", "kill -9 $$").Run()
	j, err := e.applier().Apply(context.Background(), p)
	inc := wantIncomplete(t, err)
	var ae *AdminCreateError
	if !errors.As(err, &ae) || !j.Complete || !listed(inc.Uncertain, "admin create") ||
		!strings.Contains(err.Error(), "May have changed") || !strings.Contains(err.Error(), "Running the same command again is safe") {
		t.Fatalf("%v complete=%v %+v", err, j.Complete, inc)
	}
}

func TestApplyAdminAloneCleanExitStaysPlain(t *testing.T) {
	root := stockHost(t)
	e := newApplyEnv(t, root)
	p := adminOnlyPlan(t, e, root)
	e.runner.fail["admin create"] = exec.Command("/bin/sh", "-c", "exit 1").Run()
	_, err := e.applier().Apply(context.Background(), p)
	wantNotIncomplete(t, err)
	var ae *AdminCreateError
	if !errors.As(err, &ae) {
		t.Fatal(err)
	}
}

// A journal that cannot be saved with nothing changed says so and names
// the rollback.
func TestSaveFailedNothingChanged(t *testing.T) {
	root := stockHost(t)
	a := newApplyEnv(t, root).applier()
	j := &Journal{ID: "20261123T000000Z", Command: "apply", Entries: []Entry{{Action: Action{Type: ActMkdir, File: "/x"}}}}
	err := a.saveFailed(j, make([]effect, 1), errors.New("disk full"))
	wantNotIncomplete(t, err)
	if !strings.Contains(err.Error(), "the host's configuration was not changed") || !strings.Contains(err.Error(), "skep-setup rollback "+j.ID) || !strings.Contains(err.Error(), "disk full") {
		t.Fatal(err)
	}
}

// A journal that cannot be created stops the run before any action, and
// the message says so.
func TestJournalCreationFailureSaysNothingChanged(t *testing.T) {
	e, _, _ := appliedHost(t)
	e.clock = e.clock.Add(60e9)
	p := computeRemove(t, RemoveOptions{Root: e.root}, facts(), e.runner)
	before := len(e.runner.calls)
	jr := filepath.Join(e.root, journalRoot)
	if err := os.Chmod(jr, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(jr, 0o700) })
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode")
	}
	_, err := e.remover().Apply(context.Background(), p)
	wantNotIncomplete(t, err)
	if !strings.Contains(err.Error(), "could not create the journal") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("message: %v", err)
	}
	if len(e.runner.calls) != before {
		t.Fatalf("commands ran: %v", e.runner.calls[before:])
	}
}

// A run cancelled before its first action names that point, not a failure
// of the action that never ran.
func TestCancelledBeforeActionDoesNotBlameIt(t *testing.T) {
	e, _, _ := appliedHost(t)
	e.clock = e.clock.Add(60e9)
	p := computeRemove(t, RemoveOptions{Root: e.root, RevertHost: true}, facts(), e.runner)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.remover().Apply(ctx, p)
	wantNotIncomplete(t, err)
	msg := err.Error()
	if !strings.Contains(msg, "before") || !strings.Contains(msg, "did not run") || strings.Contains(msg, " failed:") {
		t.Fatalf("message: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cause lost: %v", err)
	}
}

// A run cancelled with nothing changed whose journal could not be saved
// still leaves the journal directory, so the message names the rollback.
func TestCancelledSaveFailedNamesTheRollback(t *testing.T) {
	root := stockHost(t)
	a := newApplyEnv(t, root).applier()
	j := &Journal{ID: "20261123T000000Z", Command: "apply", Entries: []Entry{{Action: Action{Type: ActMkdir, File: "/x"}}}}
	err := a.cancelled(j, 0, make([]effect, 1), context.Canceled) // the journal's directory does not exist, so the save fails
	wantNotIncomplete(t, err)
	msg := err.Error()
	if !strings.Contains(msg, "could not be saved") || !strings.Contains(msg, "skep-setup rollback "+j.ID) ||
		!strings.Contains(msg, "the host's configuration was not changed") || strings.Contains(msg, "nothing was changed") {
		t.Fatalf("message: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cause lost: %v", err)
	}
}

// A failed action with nothing changed whose journal could not be saved
// still leaves the journal directory, so the message names the rollback.
func TestFailSaveFailedNamesTheRollback(t *testing.T) {
	root := stockHost(t)
	a := newApplyEnv(t, root).applier()
	j := &Journal{ID: "20261123T000000Z", Command: "apply", Entries: []Entry{{Action: Action{Type: ActMkdir, File: "/x"}}}}
	cause := errors.New("boom")
	err := a.fail(j, 0, make([]effect, 1), cause) // the journal's directory does not exist, so the save fails
	wantNotIncomplete(t, err)
	msg := err.Error()
	if !strings.Contains(msg, "could not be saved") || !strings.Contains(msg, "skep-setup rollback "+j.ID) ||
		!strings.Contains(msg, "the host's configuration was not changed") {
		t.Fatalf("message: %v", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("cause lost: %v", err)
	}
}

// killRunner fails "service NAME stop" the way a killed command does.
type killRunner struct {
	*fakeRunner
	err error
}

func (k killRunner) Run(ctx context.Context, c Cmd) (string, error) {
	if len(c.Args) == 2 && c.Args[1] == "stop" {
		return "", k.err
	}
	return k.fakeRunner.Run(ctx, c)
}

func TestKilledStopSaysItMayHaveTakenEffect(t *testing.T) {
	killedErr := exec.Command("/bin/sh", "-c", "kill -9 $$").Run()
	f := newFakeRunner()
	f.running["skep_api"] = true
	_, err := stopIfRunning(context.Background(), killRunner{f, killedErr}, "skep_api", func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "was killed and may have taken effect") {
		t.Fatalf("message: %v", err)
	}
	clean := exec.Command("/bin/sh", "-c", "exit 3").Run()
	_, err = stopIfRunning(context.Background(), killRunner{f, clean}, "skep_api", func(string, ...any) {})
	if err == nil || strings.Contains(err.Error(), "killed") {
		t.Fatalf("a clean failure described as killed: %v", err)
	}
}

func TestHostRunnerDropsAnEmptyStderrTail(t *testing.T) {
	_, err := HostRunner{}.Run(context.Background(), Cmd{Name: "/bin/sh", Args: []string{"-c", "kill -9 $$"}})
	if err == nil || strings.HasSuffix(err.Error(), ": ") || !strings.HasSuffix(err.Error(), "signal: killed") {
		t.Fatalf("message: %q", err)
	}
	_, err = HostRunner{}.Run(context.Background(), Cmd{Name: "/bin/sh", Args: []string{"-c", "echo why >&2; exit 3"}})
	if err == nil || !strings.HasSuffix(err.Error(), ": why") {
		t.Fatalf("stderr lost: %q", err)
	}
}
