package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner emulates sysrc(8) on the fixture files and records every
// other command. It never runs anything.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []string
	running map[string]bool
	// fail makes a command whose string contains the key fail.
	fail map[string]error
	// lie makes sysrc write this value instead of the one asked for.
	lie string
	// outputs answers a command whose string contains the key.
	outputs map[string]string
	// onStart runs after "service NAME start".
	onStart func(name string)
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{running: map[string]bool{}, fail: map[string]error{}}
}

// shortCmd is how the fakes record a command. Every program must be given
// by absolute path; base-system programs are recorded by their name.
func shortCmd(c Cmd) (string, error) {
	if !filepath.IsAbs(c.Name) {
		return "", fmt.Errorf("%q is not run by absolute path", c.Name)
	}
	name := c.Name
	for _, d := range []string{"/sbin/", "/bin/", "/usr/sbin/", "/usr/bin/"} {
		if strings.HasPrefix(name, d) && !strings.Contains(strings.TrimPrefix(name, d), "/") {
			name = strings.TrimPrefix(name, d)
		}
	}
	s := strings.Join(append([]string{name}, c.Args...), " ")
	if c.Interactive {
		s += " (interactive)"
	}
	if c.As != nil {
		s += fmt.Sprintf(" (as %d:%d)", c.As.UID, c.As.GID)
	}
	return s, nil
}

func (f *fakeRunner) Run(_ context.Context, c Cmd) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, err := shortCmd(c)
	if err != nil {
		f.calls = append(f.calls, "RELATIVE "+c.String())
		return "", err
	}
	f.calls = append(f.calls, s)
	for k, err := range f.fail {
		if strings.Contains(s, k) {
			return "", err
		}
	}
	for k, out := range f.outputs {
		if strings.Contains(s, k) {
			return out, nil
		}
	}
	switch c.Name {
	case cmdSysrc:
		return "", f.sysrc(c.Args)
	case cmdService:
		name, verb := c.Args[0], c.Args[1]
		switch verb {
		case "status":
			if !f.running[name] {
				return "", errors.New(name + " is not running")
			}
		case "start":
			f.running[name] = true
			if f.onStart != nil {
				f.onStart(name)
			}
		case "stop":
			f.running[name] = false
		}
	}
	return "", nil
}

var sysrcArgRE = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)(\+=|-=|=)(.*)$`)

// sysrc handles "-f FILE NAME=V", "NAME+=V", "NAME-=V" and "-f FILE -x NAME"
// as sysrc does: the last assignment is rewritten in place, a new one is
// appended, += adds missing words, -= removes words, -x deletes the
// variable.
func (f *fakeRunner) sysrc(args []string) error {
	if len(args) < 3 || args[0] != "-f" {
		return fmt.Errorf("fake sysrc: unsupported %v", args)
	}
	file := args[1]
	// Like FreeBSD 15.1's sysrc: a name with a dot (a loader tunable) is
	// refused, for -x as for an assignment.
	for _, a := range args[2:] {
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		name = strings.TrimRight(name, "+-")
		if strings.Contains(name, ".") {
			return fmt.Errorf("sysrc: %s: name contains characters not allowed in shell", name)
		}
	}
	b, err := os.ReadFile(file)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(b) == 0 {
		lines = nil
	}
	last := func(name string) int {
		for i := len(lines) - 1; i >= 0; i-- {
			if strings.HasPrefix(strings.TrimSpace(lines[i]), name+"=") {
				return i
			}
		}
		return -1
	}
	if args[2] == "-x" {
		name := args[3]
		var out []string
		for _, l := range lines {
			if !strings.HasPrefix(strings.TrimSpace(l), name+"=") {
				out = append(out, l)
			}
		}
		if len(out) == 0 {
			return os.WriteFile(file, nil, 0o644)
		}
		return os.WriteFile(file, []byte(strings.Join(out, "\n")+"\n"), 0o644)
	}
	m := sysrcArgRE.FindStringSubmatch(args[2])
	if m == nil {
		return fmt.Errorf("fake sysrc: bad argument %q", args[2])
	}
	name, op, val := m[1], m[2], m[3]
	i := last(name)
	cur := ""
	if i >= 0 {
		cur, _ = unquote(strings.SplitN(lines[i], "=", 2)[1])
	}
	words := strings.Fields(cur)
	switch op {
	case "+=":
		for _, w := range strings.Fields(val) {
			if !slices.Contains(words, w) {
				words = append(words, w)
			}
		}
		val = strings.Join(words, " ")
	case "-=":
		var keep []string
		for _, w := range words {
			if !slices.Contains(strings.Fields(val), w) {
				keep = append(keep, w)
			}
		}
		val = strings.Join(keep, " ")
	}
	if f.lie != "" {
		val = f.lie
	}
	line := name + `="` + val + `"`
	if i >= 0 {
		lines[i] = line
	} else {
		lines = append(lines, line)
	}
	return os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

type chownCall struct {
	path     string
	uid, gid int
}

type applyEnv struct {
	root   string
	runner *fakeRunner
	chowns []chownCall
	clock  time.Time
}

func newApplyEnv(t *testing.T, root string) *applyEnv {
	return &applyEnv{root: root, runner: newFakeRunner(), clock: time.Date(2026, 11, 22, 16, 0, 0, 0, time.UTC)}
}

func (e *applyEnv) applier() *Applier {
	return &Applier{Root: e.root, Runner: e.runner, SkepVersion: "test", Sleep: func(time.Duration) {}, AdminWait: 3 * time.Second,
		Now: func() time.Time { e.clock = e.clock.Add(time.Second); return e.clock },
		Chown: func(p string, uid, gid int) error {
			rel, _ := filepath.Rel(e.root, p)
			e.chowns = append(e.chowns, chownCall{"/" + filepath.ToSlash(filepath.Dir(rel)), uid, gid})
			return nil
		}}
}

func newBridgeOpts(root string) Options {
	return Options{Root: root, Mgmt: []string{"vtnet0"}, CreateBridge: "bridge0", Uplink: "vtnet1", Pools: []string{"tank"}}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApplyRollbackRoundTrip(t *testing.T) {
	root := stockHost(t)
	rc0, loader0 := read(t, filepath.Join(root, "etc/rc.conf")), read(t, filepath.Join(root, "boot/loader.conf"))
	e := newApplyEnv(t, root)
	p := compute(t, newBridgeOpts(root), facts())
	j, err := e.applier().Apply(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !j.Complete || j.FinishedAt == nil || len(j.Entries) != len(p.Actions()) {
		t.Fatalf("journal %+v", j)
	}
	rc, err := ReadShVars(filepath.Join(root, "etc/rc.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"kld_list": "if_epair netgraph ng_socket ng_ether ng_bpf", "zfs_enable": "YES", "cloned_interfaces": "bridge0",
		"ifconfig_vtnet1": "up", "ifconfig_bridge0": "addm vtnet1 ifuntagged vtnet1 1 defuntagged 1 vlanfilter up",
		"skep_agent_enable": "YES", "skep_api_enable": "YES", "ifconfig_vtnet0": "DHCP", "hostname": "skep1",
	} {
		if rc.Vals[k] != v {
			t.Errorf("rc.conf %s=%q, want %q", k, rc.Vals[k], v)
		}
	}
	loader, _ := ReadShVars(filepath.Join(root, "boot/loader.conf"))
	if loader.Vals["vmm_load"] != "YES" || loader.Vals["nmdm_load"] != "YES" || loader.Vals["kern.racct.enable"] != "1" {
		t.Errorf("loader.conf %v", loader.Vals)
	}
	// Files with their modes and owners.
	for path, mode := range map[string]os.FileMode{agentJSONPath: 0o600, apiJSONPath: 0o640} {
		st, err := os.Stat(hostPath(root, path))
		if err != nil || st.Mode().Perm() != mode {
			t.Errorf("%s: %v %v", path, st, err)
		}
	}
	if st, err := os.Stat(hostPath(root, mediaDir)); err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Errorf("media dir: %v %v", st, err)
	}
	// /boot: loader.conf, rewritten by setup for kern.racct.enable with
	// its owner kept (the fixture's, as the tests do not run as root).
	wantChowns := []chownCall{{"/boot", os.Getuid(), os.Getgid()}, {"/usr/local/etc/skep", 0, 0}, {"/usr/local/etc/skep", 0, 409}, {"/var/db/skep", 0, 0}}
	if fmt.Sprint(e.chowns) != fmt.Sprint(wantChowns) {
		t.Errorf("chowns %v, want %v", e.chowns, wantChowns)
	}
	if !strings.Contains(read(t, hostPath(root, apiJSONPath)), `"listen_address": "192.0.2.10:8443"`) {
		t.Error("api.json lacks the listen address")
	}
	// The journal and its backups.
	dir := JournalDir(root, j.ID)
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("journal dir mode %o", st.Mode().Perm())
	}
	if got := read(t, filepath.Join(dir, "files/etc_rc.conf")); got != rc0 {
		t.Error("the rc.conf backup is not the original")
	}
	if st, _ := os.Stat(filepath.Join(dir, "files/boot_loader.conf")); st.Mode().Perm() != 0o600 {
		t.Error("backup mode")
	}
	if strings.Contains(read(t, filepath.Join(dir, "journal.json")), "listen_address") {
		t.Error("the journal holds file content")
	}
	// No command ran but sysrc: no ifconfig, no kldload, no service.
	for _, c := range e.runner.calls {
		if !strings.HasPrefix(c, "sysrc -f "+root) {
			t.Errorf("unexpected command %q", c)
		}
	}

	// Idempotent: a second plan has nothing to do.
	p2 := compute(t, newBridgeOpts(root), facts())
	if acts := p2.Actions(); len(acts) != 0 || p2.Refused() {
		t.Fatalf("second plan: %v refused=%v", actions(p2), p2.Refused())
	}
	if _, err := e.applier().Apply(context.Background(), p2); !errors.Is(err, ErrNothingToDo) {
		t.Fatalf("second apply: %v", err)
	}
	if js, _ := LoadJournals(root); len(js) != 1 {
		t.Fatalf("%d journals after a no-op apply", len(js))
	}

	// Rollback restores the host files byte for byte and removes what
	// setup created.
	r, err := Rollback(root, "", e.clock)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(root, "etc/rc.conf")) != rc0 || read(t, filepath.Join(root, "boot/loader.conf")) != loader0 {
		t.Fatal("rollback did not restore rc.conf and loader.conf")
	}
	for _, p := range []string{agentJSONPath, apiJSONPath, mediaDir} {
		if _, err := os.Stat(hostPath(root, p)); !os.IsNotExist(err) {
			t.Errorf("%s still there after rollback", p)
		}
	}
	if len(r.Restored) != 2 || len(r.Removed) != 3 {
		t.Errorf("report %+v", r)
	}
	if _, err := Rollback(root, j.ID, e.clock); err == nil {
		t.Error("rolled back twice")
	}
	if p3 := compute(t, newBridgeOpts(root), facts()); p3.Refused() || len(p3.Actions()) != len(p.Actions()) {
		t.Errorf("after rollback the plan is not the original one")
	}
}

func TestRollbackKeepsFilesChangedSince(t *testing.T) {
	root := stockHost(t)
	loader0 := read(t, filepath.Join(root, "boot/loader.conf"))
	e := newApplyEnv(t, root)
	if _, err := e.applier().Apply(context.Background(), compute(t, newBridgeOpts(root), facts())); err != nil {
		t.Fatal(err)
	}
	appendFile(t, filepath.Join(root, "etc/rc.conf"), "sendmail_enable=\"NONE\"\n")
	appendFile(t, hostPath(root, agentJSONPath), " ")
	r, err := Rollback(root, "", e.clock)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, filepath.Join(root, "etc/rc.conf")), "sendmail_enable") ||
		!strings.Contains(read(t, filepath.Join(root, "etc/rc.conf")), "kld_list") {
		t.Fatal("a file changed since apply was overwritten")
	}
	if _, err := os.Stat(hostPath(root, agentJSONPath)); err != nil {
		t.Fatal("an agent.json changed since apply was removed")
	}
	if read(t, filepath.Join(root, "boot/loader.conf")) != loader0 {
		t.Fatal("loader.conf, unchanged since, was not restored")
	}
	if !slices.ContainsFunc(r.Kept, func(s string) bool { return strings.Contains(s, "/etc/rc.conf: changed since") }) {
		t.Errorf("report %+v", r)
	}
}

func TestApplyFailureLeavesIncompleteJournal(t *testing.T) {
	root := stockHost(t)
	loader0 := read(t, filepath.Join(root, "boot/loader.conf"))
	rc0 := read(t, filepath.Join(root, "etc/rc.conf"))
	e := newApplyEnv(t, root)
	e.runner.fail["kld_list+="] = errors.New("sysrc: boom")
	j, err := e.applier().Apply(context.Background(), compute(t, newBridgeOpts(root), facts()))
	if err == nil || !strings.Contains(err.Error(), "skep-setup rollback "+j.ID) {
		t.Fatalf("err %v", err)
	}
	if j.Complete {
		t.Fatal("journal complete after a failure")
	}
	// Nothing after the failing action ran.
	for _, c := range e.runner.calls {
		if strings.Contains(c, "zfs_enable") || strings.Contains(c, "bridge0") {
			t.Fatalf("ran %q after the failure", c)
		}
	}
	if _, err := os.Stat(hostPath(root, agentJSONPath)); !os.IsNotExist(err) {
		t.Fatal("wrote agent.json after the failure")
	}
	p := compute(t, newBridgeOpts(root), facts())
	want(t, p, Refuse, "apply "+j.ID, "rollback")
	if _, err := e.applier().Apply(context.Background(), p); err == nil {
		t.Fatal("applied over an incomplete journal")
	}
	if _, err := Rollback(root, j.ID, e.clock); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(root, "boot/loader.conf")) != loader0 || read(t, filepath.Join(root, "etc/rc.conf")) != rc0 {
		t.Fatal("rollback after a failure did not restore the files")
	}
	if compute(t, newBridgeOpts(root), facts()).Refused() {
		t.Fatal("still refused after the rollback")
	}
}

func TestApplyChecksSysrcResult(t *testing.T) {
	root := stockHost(t)
	e := newApplyEnv(t, root)
	e.runner.lie = "something else"
	_, err := e.applier().Apply(context.Background(), compute(t, newBridgeOpts(root), facts()))
	if err == nil || !strings.Contains(err.Error(), "after sysrc") {
		t.Fatalf("err %v", err)
	}
}

func TestApplyNeverOverwritesAFileThatAppeared(t *testing.T) {
	root := stockHost(t)
	e := newApplyEnv(t, root)
	p := compute(t, newBridgeOpts(root), facts())
	appendFile(t, hostPath(root, agentJSONPath), "mine\n")
	_, err := e.applier().Apply(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "appeared") {
		t.Fatalf("err %v", err)
	}
	if read(t, hostPath(root, agentJSONPath)) != "mine\n" {
		t.Fatal("overwrote the administrator's file")
	}
}

func TestApplyRefusesARefusedPlan(t *testing.T) {
	root := stockHost(t)
	f := facts()
	f.Arch = "aarch64"
	rc0 := read(t, filepath.Join(root, "etc/rc.conf"))
	e := newApplyEnv(t, root)
	if _, err := e.applier().Apply(context.Background(), compute(t, newBridgeOpts(root), f)); err == nil {
		t.Fatal("applied a refused plan")
	}
	if read(t, filepath.Join(root, "etc/rc.conf")) != rc0 || len(e.runner.calls) != 0 {
		t.Fatal("changed something")
	}
	if _, err := os.Stat(filepath.Join(root, journalRoot)); !os.IsNotExist(err) {
		t.Fatal("made a journal for a refused plan")
	}
}

// apiStarts makes the fake "service skep_api start" do what skep_api does:
// write its pid file and create api.db.
func apiStarts(root string, r *fakeRunner) {
	r.onStart = func(name string) {
		if name == "skep_api" {
			os.MkdirAll(filepath.Dir(hostPath(root, apiPID)), 0o755)
			os.WriteFile(hostPath(root, apiPID), []byte("1234\n"), 0o644)
			os.MkdirAll(hostPath(root, "/var/db/skep/api"), 0o700)
			os.WriteFile(hostPath(root, "/var/db/skep/api/api.db"), nil, 0o600)
		}
	}
}

func TestApplyStartAndAdmin(t *testing.T) {
	root := stockHost(t)
	e := newApplyEnv(t, root)
	apiStarts(root, e.runner)
	e.runner.running["skep_agent"] = true // running already: not started again
	o := newBridgeOpts(root)
	o.Start, o.Admin = true, "andy"
	if _, err := e.applier().Apply(context.Background(), compute(t, o, facts())); err != nil {
		t.Fatal(err)
	}
	var tail []string
	for _, c := range e.runner.calls {
		if !strings.HasPrefix(c, "sysrc") {
			tail = append(tail, c)
		}
	}
	want := []string{"service skep_agent status", "service skep_api status", "service skep_api start",
		root + "/usr/local/sbin/skep-api admin create --username andy (interactive) (as 409:409)"}
	if strings.Join(tail, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands:\n%s", strings.Join(tail, "\n"))
	}
	r, err := Rollback(root, "", e.clock)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(r.Kept, "\n")
	if !strings.Contains(joined, "does not stop services") || !strings.Contains(joined, "does not delete users") {
		t.Errorf("report %+v", r)
	}
}

// TestSecondApplyWithStartAndAdminIsANoOp: the same answers again, with
// the services running, plan nothing and write no journal (found on
// smith, 2026-11-22: the second apply started the services again and
// failed at "user admin already exists", leaving an incomplete journal).
func TestSecondApplyWithStartAndAdminIsANoOp(t *testing.T) {
	root := stockHost(t)
	e := newApplyEnv(t, root)
	apiStarts(root, e.runner)
	o := newBridgeOpts(root)
	o.Start, o.Admin = true, "andy"
	if _, err := e.applier().Apply(context.Background(), compute(t, o, facts())); err != nil {
		t.Fatal(err)
	}
	f := facts()
	f.Running = map[string]bool{"skep_agent": true, "skep_api": true}
	p := compute(t, o, f)
	if acts := p.Actions(); len(acts) != 0 {
		t.Fatalf("second plan has actions: %v", acts)
	}
	want(t, p, OK, "skep_agent", "running")
	want(t, p, OK, "first administrator andy", "created by skep-setup run")
	if _, err := e.applier().Apply(context.Background(), p); !errors.Is(err, ErrNothingToDo) {
		t.Fatalf("second apply: %v, want ErrNothingToDo", err)
	}
	// Rolled back, the run still made the user: not planned again.
	if _, err := Rollback(root, "", e.clock); err != nil {
		t.Fatal(err)
	}
	want(t, compute(t, o, f), OK, "first administrator andy", "")
	// Stopped services are started again.
	want(t, compute(t, o, facts()), Add, "start skep_api", "")
}

func TestPlanAdminNeedsTheAPIDirectory(t *testing.T) {
	root := stockHost(t)
	want(t, compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}, Admin: "andy"}, facts()), Refuse, "first administrator andy", "--start")
	os.MkdirAll(hostPath(root, "/var/db/skep/api"), 0o700)
	want(t, compute(t, Options{Root: root, Mgmt: []string{"vtnet0"}, Admin: "andy"}, facts()), Add, "first administrator andy", "")
}

func TestRollbackUnknownJournal(t *testing.T) {
	root := stockHost(t)
	if _, err := Rollback(root, "", time.Now()); err == nil {
		t.Error("rolled back nothing")
	}
	if _, err := Rollback(root, "20260101T000000Z", time.Now()); err == nil {
		t.Error("rolled back a missing journal")
	}
	if _, err := LoadJournal(root, "../../etc"); err == nil {
		t.Error("loaded a journal outside the journal directory")
	}
}

// TestEditTunable covers the loader.conf lines sysrc(8) cannot edit
// because their names contain dots (seen on FreeBSD 15.1, smith,
// 2026-11-22).
func TestEditTunable(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "loader.conf")
	orig := "hw.vmm.vmx.cap.virtual_interrupt_delivery=\"0\"\nvmm_load=\"YES\"" // no final newline
	if err := os.WriteFile(f, []byte(orig), 0o640); err != nil {
		t.Fatal(err)
	}
	a := &Applier{Root: "/", Chown: func(string, int, int) error { return nil }}
	set := Action{Type: ActSysrc, File: "/boot/loader.conf", Name: "kern.racct.enable", Op: "=", Value: "1"}
	if err := a.editTunable(f, set); err != nil {
		t.Fatal(err)
	}
	if got, want := read(t, f), orig+"\nkern.racct.enable=\"1\"\n"; got != want {
		t.Fatalf("after =: %q, want %q", got, want)
	}
	if st, _ := os.Stat(f); st.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want 0640 kept", st.Mode())
	}
	if err := a.editTunable(f, set); err == nil || !strings.Contains(err.Error(), "set already") {
		t.Errorf("second =: %v, want a refusal", err)
	}
	bad := set
	bad.Name, bad.Value = "kern.other", `1" ; x="`
	if err := a.editTunable(f, bad); err == nil {
		t.Error("a value with a quote was written")
	}
	bad = set
	bad.Name = "kern.racct.enable; reboot"
	if err := a.editTunable(f, bad); err == nil {
		t.Error("a name that is not a tunable was accepted")
	}
	// A similar name stays when the tunable is deleted.
	if err := os.WriteFile(f, []byte(read(t, f)+"kern.racct.enable_x=\"1\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := a.editTunable(f, Action{Type: ActSysrc, File: "/boot/loader.conf", Name: "kern.racct.enable", Op: "-x"}); err != nil {
		t.Fatal(err)
	}
	if got, want := read(t, f), orig+"\nkern.racct.enable_x=\"1\"\n"; got != want {
		t.Fatalf("after -x: %q, want %q", got, want)
	}
	if !SysrcCanEdit("kld_list") || SysrcCanEdit("kern.racct.enable") {
		t.Error("SysrcCanEdit")
	}
}

// A4 and A13: without skep_api's pid file and api.db, the administrator is
// not created; the wait is bounded, the journal is finished with the
// error, and the same apply then plans only the administrator.
func TestApplyAdminWaitsForTheAPIAndFailsCleanly(t *testing.T) {
	root := stockHost(t)
	e := newApplyEnv(t, root)
	o := newBridgeOpts(root)
	o.Start, o.Admin = true, "andy"
	sleeps := 0
	a := e.applier()
	a.Sleep = func(time.Duration) { sleeps++ }
	j, err := a.Apply(context.Background(), compute(t, o, facts()))
	var ae *AdminCreateError
	if !errors.As(err, &ae) || !strings.Contains(err.Error(), "has not written /var/run/skep/api.pid and created /var/db/skep/api/api.db within 3s") {
		t.Fatalf("err %v", err)
	}
	if sleeps != 3 {
		t.Errorf("slept %d s, want 3", sleeps)
	}
	for _, c := range e.runner.calls {
		if strings.Contains(c, "admin create") {
			t.Fatalf("ran %q before the API was up", c)
		}
	}
	if !j.Complete || j.FinishedAt == nil {
		t.Fatal("the journal is left unfinished")
	}
	last := j.Entries[len(j.Entries)-1]
	if last.Action.Type != ActAdminCreate || last.Done || !strings.Contains(last.Error, "has not written") {
		t.Fatalf("admin entry %+v", last)
	}
	// Not refused; the retry plans only the administrator.
	f := facts()
	f.Running = map[string]bool{"skep_agent": true, "skep_api": true}
	p := compute(t, o, f)
	if p.Refused() || strings.Join(actions(p), "|") != (Action{Type: ActAdminCreate, Username: "andy"}).String() {
		t.Fatalf("retry plan: refused=%v %v", p.Refused(), actions(p))
	}
	apiStarts(root, e.runner)
	e.runner.onStart("skep_api")
	if _, err := e.applier().Apply(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if c := e.runner.calls[len(e.runner.calls)-1]; !strings.HasSuffix(c, "admin create --username andy (interactive) (as 409:409)") {
		t.Fatalf("last command %q", c)
	}
}

// A4: never as root, even if the skep account says UID 0.
func TestApplyAdminRefusesARootSkepAccount(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("skep:*:0:0::/nonexistent:/usr/sbin/nologin\n"), 0o644)
	a := &Applier{Root: root, Runner: newFakeRunner()}
	if err := a.adminCreate(context.Background(), "andy"); err == nil || !strings.Contains(err.Error(), "refusing to run skep-api admin create") {
		t.Fatalf("err %v", err)
	}
}

// A13: a journal directory without a readable journal.json is an
// unfinished run with no recorded changes; it does not break every
// command, and rollback ID clears it.
func TestUnreadableJournal(t *testing.T) {
	for name, content := range map[string]*string{"missing": nil, "not JSON": ptr(`{"format": 1, "id": "2026`)} {
		t.Run(name, func(t *testing.T) {
			root := stockHost(t)
			id := "20261122T120000Z"
			os.MkdirAll(filepath.Join(JournalDir(root, id), "files"), 0o700)
			if content != nil {
				os.WriteFile(filepath.Join(JournalDir(root, id), "journal.json"), []byte(*content), 0o600)
			}
			js, err := LoadJournals(root)
			if err != nil || len(js) != 1 || js[0].Unreadable == "" || js[0].Complete {
				t.Fatalf("journals %+v, %v", js, err)
			}
			if d := js[0].Describe(); !strings.Contains(d, "unfinished, no recorded changes") {
				t.Errorf("describe %q", d)
			}
			o := Options{Root: root, Mgmt: []string{"vtnet0"}}
			want(t, compute(t, o, facts()), Refuse, "apply "+id, "rollback "+id)
			r, err := Rollback(root, id, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Kept) != 1 || !strings.Contains(r.Kept[0], "undid nothing") {
				t.Errorf("report %+v", r)
			}
			if compute(t, o, facts()).Refused() {
				t.Fatal("still refused after the rollback")
			}
			if content != nil && read(t, filepath.Join(JournalDir(root, id), "journal.json.unreadable")) != *content {
				t.Error("the damaged journal was not kept")
			}
			if _, err := Rollback(root, id, time.Now()); err == nil {
				t.Error("rolled back twice")
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// B: a variable set between the plan and the action is not overwritten;
// the action is skipped and the run still finishes.
func TestApplySkipsAVariableSetAfterThePlan(t *testing.T) {
	root := stockHost(t)
	e := newApplyEnv(t, root)
	p := compute(t, newBridgeOpts(root), facts())
	appendFile(t, filepath.Join(root, "etc/rc.conf"), "skep_api_enable=\"NO\"\n")
	var out strings.Builder
	a := e.applier()
	a.Out = &out
	j, err := a.Apply(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	rc, _ := ReadShVars(filepath.Join(root, "etc/rc.conf"))
	if rc.Vals["skep_api_enable"] != "NO" || rc.Vals["skep_agent_enable"] != "YES" {
		t.Fatalf("rc.conf %v", rc.Vals)
	}
	var skipped []Entry
	for _, en := range j.Entries {
		if en.Skipped {
			skipped = append(skipped, en)
		}
	}
	if !j.Complete || len(skipped) != 1 || skipped[0].Action.Name != "skep_api_enable" || skipped[0].Done {
		t.Fatalf("journal complete=%v skipped %+v", j.Complete, skipped)
	}
	if !strings.Contains(out.String(), `skipped: skep_api_enable was set to "NO" after the plan`) {
		t.Errorf("output:\n%s", out.String())
	}
}

// B: rollback replaces a file through a new file renamed into place,
// keeping its mode.
func TestRollbackKeepsModeAndReplacesTheFile(t *testing.T) {
	root := stockHost(t)
	rcPath := filepath.Join(root, "etc/rc.conf")
	os.Chmod(rcPath, 0o640)
	rc0 := read(t, rcPath)
	e := newApplyEnv(t, root)
	if _, err := e.applier().Apply(context.Background(), compute(t, newBridgeOpts(root), facts())); err != nil {
		t.Fatal(err)
	}
	os.Chmod(rcPath, 0o640) // the fake sysrc writes 0644
	before, _ := os.Stat(rcPath)
	if _, err := Rollback(root, "", e.clock); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(rcPath)
	if read(t, rcPath) != rc0 || after.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", after.Mode())
	}
	if os.SameFile(before, after) {
		t.Error("restored in place, not renamed into place")
	}
	entries, _ := os.ReadDir(filepath.Join(root, "etc"))
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), ".skep-setup-") {
			t.Errorf("left %s", en.Name())
		}
	}
}

// A8: the host runner gives commands a fixed environment and runs them
// from /.
func TestHostRunnerEnvironment(t *testing.T) {
	if _, err := os.Stat("/usr/bin/env"); err != nil {
		t.Skip("no /usr/bin/env")
	}
	t.Setenv("SKEP_SECRET_THING", "x")
	t.Setenv("TERM", "xterm-test")
	t.Setenv("PATH", "/nonexistent")
	out, err := HostRunner{}.Run(context.Background(), Cmd{Name: "/usr/bin/env"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(out); strings.Join(got, " ") != commandPath+" HOME=/" {
		t.Fatalf("environment %q", got)
	}
	if env := CommandEnv(true); !slices.Contains(env, "TERM=xterm-test") || len(env) != 3 {
		t.Fatalf("interactive environment %q", env)
	}
	dir, err := HostRunner{}.Run(context.Background(), Cmd{Name: "/bin/pwd"})
	if err != nil || strings.TrimSpace(dir) != "/" {
		t.Fatalf("dir %q %v", dir, err)
	}
}
