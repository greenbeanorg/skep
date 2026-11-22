package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/database"
)

// appliedHost is a stock host after a full apply, services running.
func appliedHost(t *testing.T) (*applyEnv, string, string) {
	t.Helper()
	root := stockHost(t)
	rc0, loader0 := read(t, filepath.Join(root, "etc/rc.conf")), read(t, filepath.Join(root, "boot/loader.conf"))
	e := newApplyEnv(t, root)
	if _, err := e.applier().Apply(context.Background(), compute(t, newBridgeOpts(root), facts())); err != nil {
		t.Fatal(err)
	}
	e.runner.running["skep_agent"], e.runner.running["skep_api"] = true, true
	return e, rc0, loader0
}

func computeRemove(t *testing.T, o RemoveOptions, f Facts, r Runner) Plan {
	t.Helper()
	p, err := ComputeRemove(context.Background(), o, f, r)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *applyEnv) remover() *Applier {
	a := e.applier()
	a.Command = "remove"
	return a
}

func TestRemoveKeepsDataAndHostLines(t *testing.T) {
	e, _, _ := appliedHost(t)
	p := computeRemove(t, RemoveOptions{Root: e.root}, facts(), e.runner)
	if p.Refused() {
		t.Fatalf("refused: %+v", p.Items)
	}
	got := strings.Join(actions(p), "\n")
	want := strings.Join([]string{"service skep_api stop", "service skep_agent stop",
		`sysrc skep_agent_enable="NO"`, `sysrc skep_api_enable="NO"`}, "\n")
	if got != want {
		t.Fatalf("actions:\n%s\nwant:\n%s", got, want)
	}
	j, err := e.remover().Apply(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if j.Command != "remove" || e.runner.running["skep_agent"] || e.runner.running["skep_api"] {
		t.Fatalf("journal %s, running %v", j.Command, e.runner.running)
	}
	rc, _ := ReadShVars(filepath.Join(e.root, "etc/rc.conf"))
	if rc.Vals["skep_agent_enable"] != "NO" || rc.Vals["kld_list"] == "" || rc.Vals["ifconfig_bridge0"] == "" {
		t.Fatalf("rc.conf %v", rc.Vals)
	}
	for _, p := range []string{agentJSONPath, apiJSONPath, mediaDir} {
		if _, err := os.Stat(hostPath(e.root, p)); err != nil {
			t.Errorf("%s removed without --purge-data", p)
		}
	}
	// A remove is rolled back like an apply.
	if _, err := Rollback(e.root, j.ID, e.clock); err != nil {
		t.Fatal(err)
	}
	if rc, _ := ReadShVars(filepath.Join(e.root, "etc/rc.conf")); rc.Vals["skep_agent_enable"] != "YES" {
		t.Fatal("rollback of remove did not re-enable")
	}
}

func TestRemoveRefusesWhileGuestsExist(t *testing.T) {
	e, _, _ := appliedHost(t)
	db, err := database.Open(context.Background(), hostPath(e.root, agentDB))
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE TABLE virtual_machines (id TEXT)`)
	db.Exec(`CREATE TABLE containers (id TEXT)`)
	db.Exec(`INSERT INTO containers VALUES ('j1')`)
	db.Close()
	p := computeRemove(t, RemoveOptions{Root: e.root}, facts(), e.runner)
	want(t, p, Refuse, "0 VM(s) and 1 jail(s)", "--keep-guests")
	if _, err := e.remover().Apply(context.Background(), p); err == nil {
		t.Fatal("removed with a guest")
	}
	if !e.runner.running["skep_agent"] {
		t.Fatal("stopped the agent while refusing")
	}
	p = computeRemove(t, RemoveOptions{Root: e.root, KeepGuests: true}, facts(), e.runner)
	want(t, p, Warn, "1 jail(s) are kept", "nothing will manage them")
	if p.Refused() {
		t.Fatal("refused with --keep-guests")
	}

	f := facts()
	f.VMs = []string{"skep-abc"}
	root := stockHost(t)
	want(t, computeRemove(t, RemoveOptions{Root: root}, f, newFakeRunner()), Refuse, "1 running", "--keep-guests")
}

func TestRemoveRevertHost(t *testing.T) {
	e, rc0, loader0 := appliedHost(t)
	// The operator added a module of their own and changed nothing else.
	if _, err := e.runner.Run(context.Background(), Cmd{Name: "sysrc", Args: []string{"-f", filepath.Join(e.root, "etc/rc.conf"), "kld_list+=vmm_extra"}}); err != nil {
		t.Fatal(err)
	}
	p := computeRemove(t, RemoveOptions{Root: e.root, RevertHost: true}, facts(), e.runner)
	got := strings.Join(actions(p), "\n")
	for _, w := range []string{"sysrc -f /boot/loader.conf -x vmm_load", "delete the kern.racct.enable line from /boot/loader.conf",
		`sysrc kld_list-="if_epair netgraph ng_socket ng_ether ng_bpf"`, `sysrc cloned_interfaces-="bridge0"`,
		"sysrc -x ifconfig_bridge0", "sysrc -x skep_agent_enable", "sysrc -x zfs_enable"} {
		if !strings.Contains(got, w) {
			t.Errorf("lacks %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, `skep_agent_enable="NO"`) {
		t.Error("disables a variable it removes")
	}
	want(t, p, Manual, "the running system", "ifconfig BRIDGE destroy")
	if _, err := e.remover().Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(e.root, "boot/loader.conf")) != loader0 {
		t.Errorf("loader.conf:\n%s\nwant:\n%s", read(t, filepath.Join(e.root, "boot/loader.conf")), loader0)
	}
	after, _ := ReadShVars(filepath.Join(e.root, "etc/rc.conf"))
	orig := ShVars{Vals: map[string]string{}}
	for _, l := range strings.Split(rc0, "\n") {
		if m := assignRE.FindStringSubmatch(l); m != nil {
			orig.Vals[m[1]], _ = unquote(m[2])
		}
	}
	for k, v := range orig.Vals {
		if after.Vals[k] != v {
			t.Errorf("%s=%q, was %q", k, after.Vals[k], v)
		}
	}
	if after.Vals["kld_list"] != "vmm_extra" {
		t.Errorf("kld_list %q: the operator's module must stay", after.Vals["kld_list"])
	}
	for _, k := range []string{"ifconfig_bridge0", "ifconfig_vtnet1", "zfs_enable", "skep_agent_enable", "skep_api_enable"} {
		if _, ok := after.Vals[k]; ok {
			t.Errorf("%s still set", k)
		}
	}
}

func TestRemoveRevertLeavesChangedLines(t *testing.T) {
	e, _, _ := appliedHost(t)
	e.runner.Run(context.Background(), Cmd{Name: "sysrc", Args: []string{"-f", filepath.Join(e.root, "etc/rc.conf"), "ifconfig_bridge0=addm vtnet1 addm vtnet2 up"}})
	p := computeRemove(t, RemoveOptions{Root: e.root, RevertHost: true}, facts(), e.runner)
	want(t, p, Conflict, "ifconfig_bridge0", "changed since, left alone")
	if strings.Contains(strings.Join(actions(p), "\n"), "-x ifconfig_bridge0") {
		t.Fatal("removes a line changed since")
	}
}

func TestRemovePurgeData(t *testing.T) {
	e, _, _ := appliedHost(t)
	for _, p := range []string{"agent/agent.db", "api/api.db", "api/tls/key.pem", "backup/x/agent.db", "media/install.iso"} {
		appendFile(t, hostPath(e.root, dataRoot+"/"+p), "x")
	}
	p := computeRemove(t, RemoveOptions{Root: e.root, PurgeData: true}, facts(), e.runner)
	want(t, p, Warn, "--purge-data", "node identity")
	got := strings.Join(actions(p), "\n")
	for _, w := range []string{"rm -rf /var/db/skep/agent", "rm -rf /var/db/skep/api", "rm -rf /var/db/skep/backup",
		"rm -rf /var/db/skep/media", "rm -rf " + agentJSONPath, "rm -rf " + apiJSONPath, "pw userdel skep"} {
		if !strings.Contains(got, w) {
			t.Errorf("lacks %q", w)
		}
	}
	if strings.Contains(got, "/var/db/skep/setup") {
		t.Fatal("purges setup's journals")
	}
	j, err := e.remover().Apply(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"agent", "api", "backup", "media"} {
		if _, err := os.Stat(hostPath(e.root, dataRoot+"/"+d)); !os.IsNotExist(err) {
			t.Errorf("%s not purged", d)
		}
	}
	if _, err := os.Stat(JournalDir(e.root, j.ID)); err != nil {
		t.Fatal("the journal went with the data")
	}
	calls := strings.Join(e.runner.calls, "\n")
	if !strings.Contains(calls, "pw -R "+e.root+" userdel skep") || !strings.Contains(calls, "pw -R "+e.root+" groupdel skep") {
		t.Errorf("pw calls:\n%s", calls)
	}
	// The config files are kept as copies in the journal, and rollback
	// says so rather than recreating them with the wrong owner.
	r, err := Rollback(e.root, j.ID, e.clock)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(r.Kept, "\n")
	if !strings.Contains(joined, agentJSONPath+": deleted by remove; a copy is in") || !strings.Contains(joined, "cannot be restored") ||
		!strings.Contains(joined, "the skep account") {
		t.Errorf("report %+v", r)
	}
	if _, err := os.Stat(filepath.Join(JournalDir(e.root, j.ID), "files", backupName(agentJSONPath))); err != nil {
		t.Error("no copy of agent.json")
	}
}

func TestRemovePathRefusesOtherPaths(t *testing.T) {
	a := &Applier{Root: t.TempDir()}
	for _, p := range []string{"/etc", "/var/db/skep", "/var/db/skep/setup", "/var/db/skep/agent/../../../etc", "/var/db/skep/a/b", "/usr/local/etc/skep"} {
		if err := a.removePath(p); err == nil {
			t.Errorf("%s accepted", p)
		}
	}
	if err := a.pwDelete(context.Background(), "root"); err == nil {
		t.Error("would delete root")
	}
}

func TestRemoveListsDatasetsNeverDestroys(t *testing.T) {
	e, _, _ := appliedHost(t)
	e.runner.outputs = map[string]string{"zfs list -H -o name -r tank/skep": "tank/skep\ntank/skep/vm\ntank/skep/vm/abc\n"}
	p := computeRemove(t, RemoveOptions{Root: e.root}, facts(), e.runner)
	it := want(t, p, Manual, "datasets under tank/skep", "never destroys")
	if !strings.Contains(it.Detail, "tank/skep/vm/abc") {
		t.Errorf("detail %q", it.Detail)
	}
	if _, err := e.remover().Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	for _, c := range e.runner.calls {
		if strings.Contains(c, "destroy") {
			t.Fatalf("ran %q", c)
		}
	}
}

func TestRemoveRefusesAfterIncompleteJournal(t *testing.T) {
	root := stockHost(t)
	writeJournal(t, root, Journal{Format: JournalFormat, ID: "20261122T100000Z", Command: "apply"})
	want(t, computeRemove(t, RemoveOptions{Root: root}, facts(), newFakeRunner()), Refuse, "apply 20261122T100000Z", "rollback")
}

func TestRemoveDisableOverridden(t *testing.T) {
	root := stockHost(t)
	appendFile(t, filepath.Join(root, "etc/rc.conf.d/skep"), "skep_agent_enable=\"YES\"\n")
	want(t, computeRemove(t, RemoveOptions{Root: root}, facts(), newFakeRunner()), Manual, `skep_agent_enable="NO"`, "rc.conf.d/skep")
}

// TestRevertHostKeepsTheOperatorsKldListWords is ADR 0012 amendment
// 2026-11-22, item 14: a kld_list word the operator had before apply (so
// setup did not add it) survives remove --revert-host, a rollback of that
// remove, and a rollback of the apply. Both forms setup counts as present
// are covered (a bare name and NAME.ko), and a module setup never adds.
func TestRevertHostKeepsTheOperatorsKldListWords(t *testing.T) {
	const operators = "if_epair linux ng_ether.ko"
	setupHost := func(t *testing.T) (*applyEnv, string, *Journal) {
		t.Helper()
		root := stockHost(t)
		appendFile(t, filepath.Join(root, "etc/rc.conf"), `kld_list="`+operators+`"`+"\n")
		rc0 := read(t, filepath.Join(root, "etc/rc.conf"))
		e := newApplyEnv(t, root)
		j, err := e.applier().Apply(context.Background(), compute(t, newBridgeOpts(root), facts()))
		if err != nil {
			t.Fatal(err)
		}
		// Setup journals only the words it added.
		var added []string
		for _, en := range j.Entries {
			if en.Action.Name == "kld_list" {
				added = append(added, en.Action.Op+en.Action.Value)
			}
		}
		if strings.Join(added, ",") != "+=netgraph ng_socket ng_bpf" {
			t.Fatalf("journalled kld_list changes %q", added)
		}
		if got := kldList(t, root); got != operators+" netgraph ng_socket ng_bpf" {
			t.Fatalf("after apply kld_list=%q", got)
		}
		e.runner.running["skep_agent"], e.runner.running["skep_api"] = true, true
		return e, rc0, j
	}

	t.Run("remove --revert-host, then rollbacks", func(t *testing.T) {
		e, rc0, apply := setupHost(t)
		afterApply := read(t, filepath.Join(e.root, "etc/rc.conf"))
		p := computeRemove(t, RemoveOptions{Root: e.root, RevertHost: true}, facts(), e.runner)
		if got := strings.Join(actions(p), "\n"); !strings.Contains(got, `sysrc kld_list-="netgraph ng_socket ng_bpf"`) {
			t.Fatalf("revert actions:\n%s", got)
		}
		rm, err := e.remover().Apply(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		if got := kldList(t, e.root); got != operators {
			t.Fatalf("after remove --revert-host kld_list=%q, want %q", got, operators)
		}
		// Rolling back the remove puts back the file apply left.
		if _, err := Rollback(e.root, rm.ID, e.clock); err != nil {
			t.Fatal(err)
		}
		if read(t, filepath.Join(e.root, "etc/rc.conf")) != afterApply {
			t.Fatal("rollback of remove did not restore rc.conf as apply left it")
		}
		// Rolling back the apply puts back the operator's original file.
		if _, err := Rollback(e.root, apply.ID, e.clock); err != nil {
			t.Fatal(err)
		}
		if read(t, filepath.Join(e.root, "etc/rc.conf")) != rc0 || kldList(t, e.root) != operators {
			t.Fatalf("after rolling back the apply kld_list=%q", kldList(t, e.root))
		}
	})

	t.Run("rollback of the apply", func(t *testing.T) {
		e, rc0, _ := setupHost(t)
		if _, err := Rollback(e.root, "", e.clock); err != nil {
			t.Fatal(err)
		}
		if read(t, filepath.Join(e.root, "etc/rc.conf")) != rc0 || kldList(t, e.root) != operators {
			t.Fatalf("after rollback kld_list=%q", kldList(t, e.root))
		}
	})

	t.Run("remove --revert-host after a second apply", func(t *testing.T) {
		// A second apply finds nothing to do and journals nothing, so the
		// first apply's journal alone decides what is setup's.
		e, _, _ := setupHost(t)
		if _, err := e.applier().Apply(context.Background(), compute(t, newBridgeOpts(e.root), facts())); !errors.Is(err, ErrNothingToDo) {
			t.Fatalf("second apply: %v", err)
		}
		if _, err := e.remover().Apply(context.Background(), computeRemove(t, RemoveOptions{Root: e.root, RevertHost: true}, facts(), e.runner)); err != nil {
			t.Fatal(err)
		}
		if got := kldList(t, e.root); got != operators {
			t.Fatalf("kld_list=%q, want %q", got, operators)
		}
	})
}

func kldList(t *testing.T, root string) string {
	t.Helper()
	rc, err := ReadShVars(filepath.Join(root, "etc/rc.conf"))
	if err != nil {
		t.Fatal(err)
	}
	return rc.Vals["kld_list"]
}
