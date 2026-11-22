package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// noAgent is an agent that does not answer on its socket.
var noAgent = fakeAgent{buildErr: errors.New("no socket"), healthErr: errors.New("no socket")}

func computeRemove(t *testing.T, o RemoveOptions, f Facts, r Runner) Plan {
	t.Helper()
	p, err := ComputeRemove(context.Background(), o, f, r, noAgent)
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

// agentGuests gives the agent's database tables with that many VMs and
// jails.
func agentGuests(t *testing.T, root string, vms, jails int) {
	t.Helper()
	db, err := database.Open(context.Background(), hostPath(root, testAgentDB))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{`CREATE TABLE IF NOT EXISTS virtual_machines (id TEXT)`, `CREATE TABLE IF NOT EXISTS containers (id TEXT)`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < vms; i++ {
		db.Exec(`INSERT INTO virtual_machines VALUES (?)`, fmt.Sprint("v", i))
	}
	for i := 0; i < jails; i++ {
		db.Exec(`INSERT INTO containers VALUES (?)`, fmt.Sprint("j", i))
	}
}

func TestRemoveRefusesWhileGuestsExist(t *testing.T) {
	e, _, _ := appliedHost(t)
	agentGuests(t, e.root, 0, 1)
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
	if _, err := e.runner.Run(context.Background(), Cmd{Name: cmdSysrc, Args: []string{"-f", filepath.Join(e.root, "etc/rc.conf"), "kld_list+=vmm_extra"}}); err != nil {
		t.Fatal(err)
	}
	p := computeRemove(t, RemoveOptions{Root: e.root, RevertHost: true}, noPoolFacts(), e.runner)
	got := strings.Join(actions(p), "\n")
	for _, w := range []string{"sysrc -f /boot/loader.conf -x vmm_load", "delete the kern.racct.enable line from /boot/loader.conf",
		`sysrc kld_list-="if_epair"`, `sysrc kld_list-="netgraph"`, `sysrc kld_list-="ng_socket"`,
		`sysrc kld_list-="ng_ether"`, `sysrc kld_list-="ng_bpf"`, "sysrc -x cloned_interfaces",
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

// noPoolFacts is a host with no imported pool, where --revert-host may
// remove zfs_enable.
func noPoolFacts() Facts {
	f := facts()
	f.Pools = map[string]string{}
	return f
}

// TestRevertHostKeepsZFSEnableWithPools: zfs_enable stays while a pool is
// imported, and the plan says why.
func TestRevertHostKeepsZFSEnableWithPools(t *testing.T) {
	e, _, _ := appliedHost(t)
	p := computeRemove(t, RemoveOptions{Root: e.root, RevertHost: true}, facts(), e.runner)
	if strings.Contains(strings.Join(actions(p), "\n"), "zfs_enable") {
		t.Fatalf("removes zfs_enable with pools imported:\n%s", strings.Join(actions(p), "\n"))
	}
	want(t, p, OK, "zfs_enable", "pools [sick tank zroot] are imported")
	if _, err := e.remover().Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if rc, _ := ReadShVars(filepath.Join(e.root, "etc/rc.conf")); rc.Vals["zfs_enable"] != "YES" {
		t.Fatalf("zfs_enable %q", rc.Vals["zfs_enable"])
	}
}

func TestRemoveRevertLeavesChangedLines(t *testing.T) {
	e, _, _ := appliedHost(t)
	e.runner.Run(context.Background(), Cmd{Name: cmdSysrc, Args: []string{"-f", filepath.Join(e.root, "etc/rc.conf"), "ifconfig_bridge0=addm vtnet1 addm vtnet2 up"}})
	p := computeRemove(t, RemoveOptions{Root: e.root, RevertHost: true}, noPoolFacts(), e.runner)
	want(t, p, Conflict, "ifconfig_bridge0", "changed since, left alone")
	if strings.Contains(strings.Join(actions(p), "\n"), "-x ifconfig_bridge0") {
		t.Fatal("removes a line changed since")
	}
}

func TestRemovePurgeData(t *testing.T) {
	e, _, _ := appliedHost(t)
	agentGuests(t, e.root, 0, 0)
	for _, p := range []string{"api/api.db", "api/tls/key.pem", "backup/x/agent.db", "media/install.iso"} {
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
		p := computeRemove(t, RemoveOptions{Root: e.root, RevertHost: true}, noPoolFacts(), e.runner)
		if got := strings.Join(actions(p), "\n"); !strings.Contains(got, "sysrc kld_list-=\"netgraph\"\nsysrc kld_list-=\"ng_socket\"\nsysrc kld_list-=\"ng_bpf\"") {
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
		if _, err := e.remover().Apply(context.Background(), computeRemove(t, RemoveOptions{Root: e.root, RevertHost: true}, noPoolFacts(), e.runner)); err != nil {
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

// TestRevertHostRestoresTheBytes: apply then remove --revert-host on a
// stock host gives rc.conf and loader.conf back byte for byte. sysrc
// NAME-= leaves NAME="" (FreeBSD 15.1, smith, 2026-11-22), so a list
// setup created is deleted instead; an empty leftover of an earlier
// revert is deleted too.
func TestRevertHostRestoresTheBytes(t *testing.T) {
	e, rc0, loader0 := appliedHost(t)
	rcPath := filepath.Join(e.root, "etc/rc.conf")
	p := computeRemove(t, RemoveOptions{Root: e.root, RevertHost: true}, noPoolFacts(), e.runner)
	got := strings.Join(actions(p), "\n")
	for _, w := range []string{"sysrc -x kld_list", "sysrc -x cloned_interfaces"} {
		if !strings.Contains(got, w) {
			t.Errorf("lacks %q:\n%s", w, got)
		}
	}
	if _, err := e.remover().Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := read(t, rcPath); got != rc0 {
		t.Errorf("rc.conf:\n%s\nwant:\n%s", got, rc0)
	}
	if got := read(t, filepath.Join(e.root, "boot/loader.conf")); got != loader0 {
		t.Errorf("loader.conf:\n%s\nwant:\n%s", got, loader0)
	}

	// The leftover of an older build's revert: kld_list="" stays behind.
	e, rc0, _ = appliedHost(t)
	rcPath = filepath.Join(e.root, "etc/rc.conf")
	if _, err := e.runner.Run(context.Background(), Cmd{Name: cmdSysrc, Args: []string{"-f", rcPath, "kld_list-=if_epair netgraph ng_socket ng_ether ng_bpf"}}); err != nil {
		t.Fatal(err)
	}
	if v, _ := ReadShVars(rcPath); v.Vals["kld_list"] != "" {
		t.Fatalf("fake sysrc: kld_list %q", v.Vals["kld_list"])
	}
	p = computeRemove(t, RemoveOptions{Root: e.root, RevertHost: true}, noPoolFacts(), e.runner)
	if !strings.Contains(strings.Join(actions(p), "\n"), "sysrc -x kld_list") {
		t.Errorf("an empty kld_list setup created is not deleted:\n%s", strings.Join(actions(p), "\n"))
	}
	if _, err := e.remover().Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := read(t, rcPath); got != rc0 {
		t.Errorf("rc.conf after the leftover:\n%s\nwant:\n%s", got, rc0)
	}
}

// A6: remove reads the database the agent's configuration names; an
// answering agent without it, or a database without the tables, is not
// "no guests".
func TestRemoveGuestsFromTheConfiguredDatabase(t *testing.T) {
	t.Run("configured path", func(t *testing.T) {
		e, _, _ := appliedHost(t)
		// agent.json was written by apply; skep_agent_config points
		// elsewhere, as an operator might.
		appendFile(t, filepath.Join(e.root, "etc/rc.conf"), "skep_agent_config=\"/srv/agent.json\"\n")
		appendFile(t, hostPath(e.root, "/srv/agent.json"), `{"database":{"path":"/srv/db/agent.db"}}`)
		db, err := database.Open(context.Background(), hostPath(e.root, "/srv/db/agent.db"))
		if err != nil {
			t.Fatal(err)
		}
		db.Exec(`CREATE TABLE virtual_machines (id TEXT)`)
		db.Exec(`CREATE TABLE containers (id TEXT)`)
		db.Exec(`INSERT INTO virtual_machines VALUES ('v1')`)
		db.Close()
		want(t, computeRemove(t, RemoveOptions{Root: e.root}, facts(), e.runner), Refuse, "1 VM(s) and 0 jail(s)", "")
	})
	t.Run("answering agent, no database", func(t *testing.T) {
		e, _, _ := appliedHost(t)
		p, err := ComputeRemove(context.Background(), RemoveOptions{Root: e.root}, facts(), e.runner, fakeAgent{health: "ok"})
		if err != nil {
			t.Fatal(err)
		}
		want(t, p, Refuse, "the agent's database", "answers on its socket")
	})
	t.Run("no such table", func(t *testing.T) {
		e, _, _ := appliedHost(t)
		db, err := database.Open(context.Background(), hostPath(e.root, testAgentDB))
		if err != nil {
			t.Fatal(err)
		}
		db.Exec(`CREATE TABLE containers (id TEXT)`)
		db.Close()
		if _, err := ComputeRemove(context.Background(), RemoveOptions{Root: e.root}, facts(), e.runner, noAgent); err == nil ||
			!strings.Contains(err.Error(), "no such table") {
			t.Fatalf("err %v", err)
		}
	})
}

// A7: --purge-data is refused while a Skep guest runs, --keep-guests or
// not.
func TestRemovePurgeRefusedWhileGuestsRun(t *testing.T) {
	e, _, _ := appliedHost(t)
	agentGuests(t, e.root, 0, 1)
	f := facts()
	f.Jails = []string{"skep-j0", "www"}
	for _, keep := range []bool{false, true} {
		p := computeRemove(t, RemoveOptions{Root: e.root, PurgeData: true, KeepGuests: keep}, f, e.runner)
		want(t, p, Refuse, "--purge-data", "--keep-guests does not change this")
	}
	// Kept guests that are not running: allowed with --keep-guests.
	if p := computeRemove(t, RemoveOptions{Root: e.root, PurgeData: true, KeepGuests: true}, facts(), e.runner); p.Refused() {
		t.Fatalf("refused: %+v", p.Items)
	}
}

// A7: an encryption key file under /var/db/skep is found; purging it is
// refused.
func TestRemoveFindsKeysUnderTheDataDirectory(t *testing.T) {
	e, _, _ := appliedHost(t)
	agentGuests(t, e.root, 0, 0)
	e.runner.outputs = map[string]string{
		"zfs list -H -o name -r tank/skep":                "tank/skep\ntank/skep/vm\n",
		"zfs get -H -o value -r encryptionroot tank/skep": "tank/skep\ntank/skep\n",
		"zfs get -H -o name,value keylocation tank/skep":  "tank/skep\tfile:///var/db/skep/keys/tank.key\n",
	}
	want(t, computeRemove(t, RemoveOptions{Root: e.root, PurgeData: true}, facts(), e.runner), Refuse, "--purge-data", "/var/db/skep/keys/tank.key")
	want(t, computeRemove(t, RemoveOptions{Root: e.root}, facts(), e.runner), Warn, "encryption keys under /var/db/skep", "would refuse")
	e.runner.outputs["zfs get -H -o name,value keylocation tank/skep"] = "tank/skep\tprompt\n"
	p := computeRemove(t, RemoveOptions{Root: e.root, PurgeData: true}, facts(), e.runner)
	want(t, p, OK, "encryption keys of tank/skep", "no key file under /var/db/skep")
	want(t, p, Warn, "--purge-data", "Skep keeps no ZFS encryption key")
	if p.Refused() {
		t.Fatalf("refused: %+v", p.Items)
	}
}

// B: each skep_*_enable gets one line in the revert plan.
func TestRevertPlanHasOneLinePerEnableVariable(t *testing.T) {
	e, _, _ := appliedHost(t)
	rcPath := filepath.Join(e.root, "etc/rc.conf")
	if _, err := e.runner.Run(context.Background(), Cmd{Name: cmdSysrc, Args: []string{"-f", rcPath, "-x", "skep_api_enable"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.runner.Run(context.Background(), Cmd{Name: cmdSysrc, Args: []string{"-f", rcPath, "skep_agent_enable=yes"}}); err != nil {
		t.Fatal(err)
	}
	p := computeRemove(t, RemoveOptions{Root: e.root, RevertHost: true}, noPoolFacts(), e.runner)
	for _, v := range []string{"skep_api_enable", "skep_agent_enable"} {
		n := 0
		for _, it := range p.Items {
			if it.Kind != Add && strings.HasPrefix(it.What, v) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s: %d non-action lines, want 1: %+v", v, n, p.Items)
		}
	}
	// A changed value that still says yes is disabled.
	if !slices.Contains(actions(p), `sysrc skep_agent_enable="NO"`) {
		t.Errorf("actions %v", actions(p))
	}
}

// B: a "-x" is made only while the variable still has the value the plan
// saw.
func TestRevertSkipsAVariableChangedAfterThePlan(t *testing.T) {
	e, _, _ := appliedHost(t)
	p := computeRemove(t, RemoveOptions{Root: e.root, RevertHost: true}, noPoolFacts(), e.runner)
	rcPath := filepath.Join(e.root, "etc/rc.conf")
	if _, err := e.runner.Run(context.Background(), Cmd{Name: cmdSysrc, Args: []string{"-f", rcPath, "ifconfig_bridge0=addm vtnet1 addm vtnet2 up"}}); err != nil {
		t.Fatal(err)
	}
	j, err := e.remover().Apply(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if rc, _ := ReadShVars(rcPath); rc.Vals["ifconfig_bridge0"] != "addm vtnet1 addm vtnet2 up" {
		t.Fatalf("ifconfig_bridge0 %q", rc.Vals["ifconfig_bridge0"])
	}
	if !slices.ContainsFunc(j.Entries, func(en Entry) bool {
		return en.Skipped && en.Action.Name == "ifconfig_bridge0" && strings.Contains(en.Error, "changed to")
	}) {
		t.Fatalf("entries %+v", j.Entries)
	}
}

// Part 0 a: when the key check cannot run, --purge-data is refused (not
// warned, not skipped); without it the warning stays.
func TestRemovePurgeRefusedWhenTheKeyCheckCannotRun(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *applyEnv)
		why   string
	}{
		{"zfs list fails", func(e *applyEnv) {
			e.runner.fail = map[string]error{"zfs list -H -o name -r tank/skep": errors.New("cannot open 'tank': I/O error")}
		}, "zfs list failed"},
		{"zfs get encryptionroot fails", func(e *applyEnv) {
			e.runner.outputs = map[string]string{"zfs list -H -o name -r tank/skep": "tank/skep\n"}
			e.runner.fail = map[string]error{"encryptionroot": errors.New("permission denied")}
		}, "zfs get encryptionroot failed"},
		{"zfs get keylocation fails", func(e *applyEnv) {
			e.runner.outputs = map[string]string{
				"zfs list -H -o name -r tank/skep":                "tank/skep\n",
				"zfs get -H -o value -r encryptionroot tank/skep": "tank/skep\n",
			}
			e.runner.fail = map[string]error{"keylocation": errors.New("permission denied")}
		}, "zfs get keylocation failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, _, _ := appliedHost(t)
			agentGuests(t, e.root, 0, 0)
			c.setup(e)
			p := computeRemove(t, RemoveOptions{Root: e.root, PurgeData: true}, facts(), e.runner)
			want(t, p, Refuse, "--purge-data", c.why)
			p = computeRemove(t, RemoveOptions{Root: e.root}, facts(), e.runner)
			want(t, p, Warn, "encryption keys under", c.why)
			if p.Refused() {
				t.Fatalf("plain remove refused: %+v", p.Items)
			}
		})
	}
}

// Part 0 a: a pool that is not imported, or a dataset root that does not
// exist, does not refuse --purge-data.
func TestRemovePurgeAllowedWithoutTheDatasetRoot(t *testing.T) {
	e, _, _ := appliedHost(t)
	agentGuests(t, e.root, 0, 0)
	e.runner.fail = map[string]error{"zfs list -H -o name -r tank/skep": errors.New("exit status 1: cannot open 'tank/skep': dataset does not exist")}
	p := computeRemove(t, RemoveOptions{Root: e.root, PurgeData: true}, facts(), e.runner)
	want(t, p, OK, "datasets under tank/skep", "does not exist")
	if p.Refused() {
		t.Fatalf("refused: %+v", p.Items)
	}
	// Without agent.json the agent has the defaults: no pools, nothing to
	// check.
	cfg := filepath.Join(e.root, "usr/local/etc/skep/agent.json")
	saved := read(t, cfg)
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	if p := computeRemove(t, RemoveOptions{Root: e.root, PurgeData: true}, facts(), e.runner); p.Refused() {
		t.Fatalf("refused without agent.json: %+v", p.Items)
	}
	if err := os.WriteFile(cfg, []byte(saved), 0o640); err != nil {
		t.Fatal(err)
	}
	f := facts()
	delete(f.Pools, "tank")
	e.runner.fail = map[string]error{}
	p = computeRemove(t, RemoveOptions{Root: e.root, PurgeData: true}, f, e.runner)
	want(t, p, Warn, "encryption keys under tank/skep", "not imported")
	if p.Refused() {
		t.Fatalf("refused: %+v", p.Items)
	}
}

// Part 0 a: agent.json that cannot be read or parsed stops remove before
// any plan (the guest count reads it first), with or without --purge-data.
func TestRemoveStopsOnAnUnreadableAgentJSON(t *testing.T) {
	for name, damage := range map[string]func(p string) error{
		"directory": func(p string) error {
			if err := os.Remove(p); err != nil {
				return err
			}
			return os.Mkdir(p, 0o755)
		},
		"broken": func(p string) error { return os.WriteFile(p, []byte("{"), 0o640) },
	} {
		t.Run(name, func(t *testing.T) {
			e, _, _ := appliedHost(t)
			agentGuests(t, e.root, 0, 0)
			if err := damage(filepath.Join(e.root, "usr/local/etc/skep/agent.json")); err != nil {
				t.Fatal(err)
			}
			for _, purge := range []bool{true, false} {
				if _, err := ComputeRemove(context.Background(), RemoveOptions{Root: e.root, PurgeData: purge}, facts(), e.runner, noAgent); err == nil {
					t.Fatalf("purge=%v: planned", purge)
				}
			}
		})
	}
}
