package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/pkg/version"
)

// upgradeHost fakes the commands upgrade runs: -version of the installed
// programs, db backup, service, and fstat.
type upgradeHost struct {
	root      string
	calls     []string
	builds    map[string]string // bin -> "skep-agent 0.1.0 (commit x, ..."
	textInode map[string]string // service -> inode its process runs
	nextPID   int
	fail      map[string]error
	noPIDFile map[string]bool
	// on runs a hook after a command containing the key was recorded.
	on map[string]func()
	// The cluster: whether it is initialised, the build its status
	// reports (the installed one once skep_cluster has started), and
	// its view; the import state skep-api prints.
	clusterInit  bool
	clusterBuild string
	view         ClusterView
	// onStatus runs before each "skep-cluster status -json" answer.
	onStatus    func()
	importState string
	// importTo is the state "admin import-users" leaves; initErr fails
	// "skep-cluster init".
	importTo string
	initErr  error
}

func (h *upgradeHost) Run(ctx context.Context, c Cmd) (string, error) {
	short, err := shortCmd(c)
	if err != nil {
		h.calls = append(h.calls, "RELATIVE "+c.String())
		return "", err
	}
	s := strings.ReplaceAll(short, h.root, "")
	// Like exec.CommandContext: a cancelled context runs nothing.
	if err := ctx.Err(); err != nil {
		h.calls = append(h.calls, "CANCELLED "+s)
		return "", err
	}
	h.calls = append(h.calls, s)
	for k, f := range h.on {
		if strings.Contains(s, k) {
			f()
		}
	}
	for k, err := range h.fail {
		if strings.Contains(s, k) {
			return "", err
		}
	}
	switch {
	case strings.HasSuffix(c.Name, clusterBin):
		switch strings.Join(c.Args, " ") {
		case "version":
			return h.builds[clusterBin] + "\n", nil
		case "init -check":
			return map[bool]string{true: "initialised\n", false: "not-initialised\n"}[h.clusterInit], nil
		case "init":
			if h.initErr != nil {
				return "", h.initErr
			}
			h.clusterInit = true
			return "This host is now a one-node cluster.\n", nil
		case "status -json":
			if _, err := os.Stat(hostPath(h.root, clusterPID)); err != nil {
				return "", errors.New("skep-cluster is not reachable on its socket")
			}
			if h.onStatus != nil {
				h.onStatus()
			}
			v := h.view
			v.Build = h.clusterBuild
			b, _ := json.Marshal(struct {
				Status ClusterView       `json:"status"`
				Health map[string]string `json:"health"`
			}{v, map[string]string{"status": v.Health}})
			return string(b), nil
		}
		return "", fmt.Errorf("fake skep-cluster: %v", c.Args)
	case strings.HasSuffix(c.Name, apiBin) && len(c.Args) > 1 && c.Args[0] == "admin":
		switch c.Args[1] {
		case "list":
			return h.importState + "\n", nil
		case "import-users":
			h.importState = h.importTo
			return "This host's 2 users are now the cluster's (operation x).\n", nil
		}
		return "", fmt.Errorf("fake skep-api: %v", c.Args)
	case c.Name == cmdSysrc:
		return "", (&fakeRunner{}).sysrc(c.Args)
	case len(c.Args) == 1 && c.Args[0] == "-version":
		return h.builds[strings.TrimPrefix(c.Name, h.root)] + "\n", nil
	case len(c.Args) > 1 && c.Args[0] == "db":
		return filepath.Base(c.Name) + " db backup: done\n", nil
	case c.Name == cmdService:
		svc, verb := c.Args[0], c.Args[1]
		pid := map[string]string{"skep_agent": agentPID, "skep_api": apiPID, "skep_cluster": clusterPID}[svc]
		switch verb {
		case "status":
			if _, err := os.Stat(hostPath(h.root, pid)); err != nil {
				return "", errors.New(svc + " is not running")
			}
		case "stop":
			os.Remove(hostPath(h.root, pid))
		case "start":
			h.nextPID++
			if svc == "skep_cluster" {
				h.clusterBuild = strings.Fields(h.builds[clusterBin])[1] + "+" + strings.Trim(strings.Fields(h.builds[clusterBin])[2], "(,")
			}
			if !h.noPIDFile[svc] {
				os.MkdirAll(filepath.Dir(hostPath(h.root, pid)), 0o755)
				os.WriteFile(hostPath(h.root, pid), []byte(fmt.Sprintf("%d\n", 1000+h.nextPID)), 0o644)
			}
		}
	case c.Name == cmdFstat:
		pid := c.Args[1]
		svc := "skep_agent"
		if b, _ := os.ReadFile(hostPath(h.root, apiPID)); strings.TrimSpace(string(b)) == pid {
			svc = "skep_api"
		}
		if b, _ := os.ReadFile(hostPath(h.root, clusterPID)); strings.TrimSpace(string(b)) == pid {
			svc = "skep_cluster"
		}
		return fmt.Sprintf("USER CMD PID FD MOUNT INUM MODE SZ|DV R/W\nroot %s %s text / %s -r-xr-xr-x 1234 r\nroot %s %s wd / 2 drwxr-xr-x 512 r\n",
			svc, pid, h.textInode[svc], svc, pid), nil
	}
	return "", nil
}

type fakeAgent struct {
	build, health       string
	buildErr, healthErr error
}

func (a fakeAgent) Build(context.Context) (string, error)  { return a.build, a.buildErr }
func (a fakeAgent) Health(context.Context) (string, error) { return a.health, a.healthErr }

// seqAgent answers Build from a sequence: an error until the sequence
// says otherwise, the old build before the restart, the new one after.
type seqAgent struct {
	builds []string // "" means no answer
	n      *int
	health string
}

func (a seqAgent) Build(context.Context) (string, error) {
	i := *a.n
	*a.n++
	if i >= len(a.builds) {
		i = len(a.builds) - 1
	}
	if a.builds[i] == "" {
		return "", errors.New("connection refused")
	}
	return a.builds[i], nil
}
func (a seqAgent) Health(context.Context) (string, error) { return a.health, nil }

var newBuild = version.Info{Version: "0.1.1", Commit: "abc1234"}

// testAgentDB is the agent's database with the stock configuration.
const testAgentDB = "/var/db/skep/agent/agent.db"

func newUpgrade(t *testing.T) (*Upgrader, *upgradeHost) {
	root := t.TempDir()
	h := &upgradeHost{root: root,
		builds: map[string]string{
			agentBin: "skep-agent 0.1.1 (commit abc1234, built 2026-11-22T16:00:00Z, go1.27.4 freebsd/amd64)",
			apiBin:   "skep-api 0.1.1 (commit abc1234, built 2026-11-22T16:00:00Z, go1.27.4 freebsd/amd64)",
			// "skep-cluster version" has its own shape (facts F3).
			clusterBin: "skep-cluster 0.1.1 (abc1234, built 2026-11-22T16:00:00Z)",
		},
		textInode: map[string]string{"skep_agent": "11", "skep_api": "22", "skep_cluster": "33"},
		fail:      map[string]error{}, noPIDFile: map[string]bool{}, on: map[string]func(){},
		// An upgraded 8c-1 host: initialised, enabled, running alone,
		// its users imported.
		clusterInit: true, clusterBuild: "0.1.0+dd7a8b6", view: aloneView(""), importState: "done", importTo: "done",
	}
	// The three services run, and the agent has its database.
	for _, pid := range []string{agentPID, apiPID, clusterPID} {
		os.MkdirAll(filepath.Dir(hostPath(root, pid)), 0o755)
		os.WriteFile(hostPath(root, pid), []byte("900\n"), 0o644)
	}
	for f, content := range map[string]string{
		"etc/passwd":  "root:*:0:0::/root:/bin/sh\nskep:*:409:409::/nonexistent:/usr/sbin/nologin\nskep-cluster:*:410:410::/nonexistent:/usr/sbin/nologin\n",
		"etc/group":   "wheel:*:0:root\nskep:*:409:\nskep-cluster:*:410:\n",
		"etc/rc.conf": "skep_agent_enable=\"YES\"\nskep_cluster_enable=\"YES\"\nskep_api_enable=\"YES\"\n",
	} {
		os.MkdirAll(filepath.Dir(filepath.Join(root, f)), 0o755)
		os.WriteFile(filepath.Join(root, f), []byte(content), 0o644)
	}
	setTasks(root)
	n := 0
	running := facts()
	running.Running = map[string]bool{"skep_agent": true, "skep_cluster": true, "skep_api": true}
	u := &Upgrader{Root: root, Runner: h, Prober: fakeProber{running},
		Agent: seqAgent{builds: []string{"0.1.0+dd7a8b6", "0.1.1+abc1234"}, n: &n, health: "ok"},
		Now:   func() time.Time { return time.Date(2026, 11, 22, 17, 0, 0, 0, time.UTC) }, Self: newBuild,
		Sleep: func(time.Duration) {}, PIDWait: 3 * time.Second,
		Inode: func(p string) (uint64, error) {
			return map[string]uint64{hostPath(root, agentBin): 11, hostPath(root, apiBin): 22, hostPath(root, clusterBin): 33}[p], nil
		}}
	return u, h
}

type fakeProber struct{ f Facts }

func (p fakeProber) Probe(context.Context) (Facts, error) { return p.f, nil }

func TestUpgradeInOrder(t *testing.T) {
	u, h := newUpgrade(t)
	if err := u.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := "/var/db/skep/backup/0.1.0+dd7a8b6-20261122T170000Z"
	want := []string{
		agentBin + " -version", apiBin + " -version", clusterBin + " version",
		clusterBin + " init -check", clusterBin + " status -json",
		"service skep_api status", "service skep_api stop",
		agentBin + " db backup -dir " + dir + " -keep 3", apiBin + " db backup -dir " + dir + " -keep 3",
		"service skep_agent status", "service skep_agent stop",
		"service skep_agent start", "fstat -p 1001", agentBin + " -version",
		// D11 step 6: skep_cluster before skep_api, verified, then a leader.
		"service skep_cluster status", "service skep_cluster stop", "service skep_cluster start",
		"fstat -p 1002", clusterBin + " version", clusterBin + " status -json",
		"service skep_api start", "fstat -p 1003", apiBin + " -version",
		apiBin + " admin list --import-state -data-dir /var/db/skep (as 409:409)",
	}
	if strings.Join(h.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands:\n%s\nwant:\n%s", strings.Join(h.calls, "\n"), strings.Join(want, "\n"))
	}
}

func serviceCalls(h *upgradeHost) []string {
	var out []string
	for _, c := range h.calls {
		if strings.HasPrefix(c, "service") {
			out = append(out, c)
		}
	}
	return out
}

func TestUpgradeRefusals(t *testing.T) {
	cases := map[string]func(u *Upgrader, h *upgradeHost){
		"platform": func(u *Upgrader, h *upgradeHost) {
			f := facts()
			f.KernelRelease, f.UserlandRelease = "14.3-RELEASE", "14.3-RELEASE"
			u.Prober = fakeProber{f}
		},
		"agent is another build": func(u *Upgrader, h *upgradeHost) {
			h.builds[agentBin] = "skep-agent 0.1.0 (commit dd7a8b6, built x, go freebsd/amd64)"
		},
		"api is another build": func(u *Upgrader, h *upgradeHost) {
			h.builds[apiBin] = "skep-api 0.1.1 (commit fff0000, built x, go freebsd/amd64)"
		},
		"unreadable version": func(u *Upgrader, h *upgradeHost) { h.builds[apiBin] = "garbage" },
		"running tasks":      func(u *Upgrader, h *upgradeHost) { setTasks(h.root, "running", "succeeded") },
		"pending task":       func(u *Upgrader, h *upgradeHost) { setTasks(h.root, "pending", "failed") },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			u, h := newUpgrade(t)
			mod(u, h)
			err := u.Upgrade(context.Background())
			var r *UpgradeRefusal
			if !errors.As(err, &r) || !strings.HasSuffix(err.Error(), "nothing was changed") {
				t.Fatalf("err %v", err)
			}
			for _, c := range h.calls {
				if strings.Contains(c, "db backup") || strings.HasPrefix(c, "service") {
					t.Fatalf("ran %q while refusing", c)
				}
			}
		})
	}
	t.Run("the refusal names --force's cost", func(t *testing.T) {
		u, h := newUpgrade(t)
		cases["pending task"](u, h)
		err := u.Upgrade(context.Background())
		if err == nil || !strings.Contains(err.Error(), "1 task(s) pending and 0 running") ||
			!strings.Contains(err.Error(), "pending and running tasks are then interrupted and must be resubmitted") {
			t.Fatalf("err %v", err)
		}
	})
}

// setTasks gives the agent's database one task per status.
func setTasks(root string, statuses ...string) {
	db, err := database.Open(context.Background(), hostPath(root, testAgentDB))
	if err != nil {
		panic(err)
	}
	defer db.Close()
	db.Exec(`CREATE TABLE IF NOT EXISTS tasks (id TEXT, status TEXT)`)
	db.Exec(`DELETE FROM tasks`)
	for i, st := range statuses {
		if _, err := db.Exec(`INSERT INTO tasks VALUES (?, ?)`, fmt.Sprintf("t%d", i), st); err != nil {
			panic(err)
		}
	}
}

// apiRestarted checks the API was started again after its stop, and
// verified: its pid file read, its process checked with fstat, its build.
func apiRestarted(t *testing.T, h *upgradeHost) {
	t.Helper()
	calls := strings.Join(h.calls, "\n")
	i := strings.LastIndex(calls, "service skep_api start")
	if i < 0 || !strings.Contains(calls, "service skep_api stop") {
		t.Fatalf("skep_api not stopped and started again:\n%s", calls)
	}
	if !strings.Contains(calls[i:], "fstat -p ") || !strings.Contains(calls[i:], apiBin+" -version") {
		t.Fatalf("restarted skep_api not verified:\n%s", calls)
	}
	if _, err := os.Stat(hostPath(h.root, apiPID)); err != nil {
		t.Fatal("no API pid file after the restart")
	}
}

func noBackupNoAgent(t *testing.T, h *upgradeHost) {
	t.Helper()
	for _, c := range h.calls {
		if strings.Contains(c, "db backup") || strings.Contains(c, "skep_agent") {
			t.Fatalf("ran %q", c)
		}
	}
}

func TestUpgradeTaskAfterTheAPIStopIsRefusedAndTheAPIRestarted(t *testing.T) {
	u, h := newUpgrade(t)
	setTasks(h.root, "succeeded")
	// A task submitted between the check and the API stop.
	h.on["service skep_api stop"] = func() { setTasks(h.root, "succeeded", "pending") }
	sleeps := 0
	u.Sleep = func(time.Duration) { sleeps++ }
	u.Wait = 5 * time.Second
	err := u.Upgrade(context.Background())
	var r *UpgradeRefusal
	if !errors.As(err, &r) {
		t.Fatalf("err %v", err)
	}
	for _, w := range []string{"1 task(s) pending and 0 running after waiting 5s with skep_api stopped",
		"must be resubmitted", "skep_api was started again", "503 agent_version_mismatch", "once the tasks have finished"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error lacks %q: %v", w, err)
		}
	}
	if strings.Contains(err.Error(), "nothing was changed") {
		t.Error("claims nothing was changed after restarting the API")
	}
	if sleeps != 5 {
		t.Errorf("waited %d s, want 5", sleeps)
	}
	noBackupNoAgent(t, h)
	apiRestarted(t, h)
}

func TestUpgradeRunningTaskAfterTheAPIStopIsRefusedWithoutWaiting(t *testing.T) {
	u, h := newUpgrade(t)
	h.on["service skep_api stop"] = func() { setTasks(h.root, "running") }
	u.Wait = -1
	u.Sleep = func(time.Duration) { t.Fatal("slept with no wait") }
	err := u.Upgrade(context.Background())
	if err == nil || !strings.Contains(err.Error(), "0 task(s) pending and 1 running after waiting 0s") {
		t.Fatalf("err %v", err)
	}
	noBackupNoAgent(t, h)
	apiRestarted(t, h)
}

func TestUpgradeProceedsWhenTasksFinishWithinTheWait(t *testing.T) {
	u, h := newUpgrade(t)
	h.on["service skep_api stop"] = func() { setTasks(h.root, "running", "pending") }
	sleeps := 0
	u.Sleep = func(time.Duration) {
		sleeps++
		switch sleeps {
		case 2:
			setTasks(h.root, "succeeded", "running")
		case 3:
			setTasks(h.root, "succeeded", "succeeded")
		}
	}
	u.Wait = 60 * time.Second
	if err := u.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sleeps != 3 {
		t.Errorf("slept %d times, want 3", sleeps)
	}
	if got := strings.Join(serviceCalls(h), ", "); got != "service skep_api status, service skep_api stop, service skep_agent status, service skep_agent stop, service skep_agent start, service skep_cluster status, service skep_cluster stop, service skep_cluster start, service skep_api start" {
		t.Fatalf("services: %s", got)
	}
}

func TestUpgradeForceInterruptsPendingAndRunning(t *testing.T) {
	u, h := newUpgrade(t)
	setTasks(h.root, "pending", "running")
	var out strings.Builder
	u.Out, u.Force, u.Wait = &out, true, 2*time.Second
	if err := u.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "--force: 1 task(s) pending and 1 running are interrupted by the agent restart and must be resubmitted") {
		t.Fatalf("output:\n%s", out.String())
	}
	if got := strings.Join(serviceCalls(h), ", "); got != "service skep_api status, service skep_api stop, service skep_agent status, service skep_agent stop, service skep_agent start, service skep_cluster status, service skep_cluster stop, service skep_cluster start, service skep_api start" {
		t.Fatalf("services: %s", got)
	}
}

func TestUpgradeRecheckFailureRestartsTheAPI(t *testing.T) {
	u, h := newUpgrade(t)
	setTasks(h.root, "succeeded")
	h.on["service skep_api stop"] = func() {
		if err := os.WriteFile(hostPath(h.root, testAgentDB), []byte("not a database, not at all, garbage garbage garbage garbage garbage garbage garbage garbage garbage garbage"), 0o600); err != nil {
			panic(err)
		}
		os.Remove(hostPath(h.root, testAgentDB) + "-wal")
		os.Remove(hostPath(h.root, testAgentDB) + "-shm")
	}
	err := u.Upgrade(context.Background())
	if err == nil || !strings.Contains(err.Error(), "read the agent's tasks after stopping skep_api") ||
		!strings.Contains(err.Error(), "skep_api was started again") {
		t.Fatalf("err %v", err)
	}
	noBackupNoAgent(t, h)
	apiRestarted(t, h)
}

func TestUpgradeRestoreFailureIsReported(t *testing.T) {
	u, h := newUpgrade(t)
	h.on["service skep_api stop"] = func() { setTasks(h.root, "pending") }
	h.fail["service skep_api start"] = errors.New("exit status 1")
	u.Wait = -1
	err := u.Upgrade(context.Background())
	if err == nil || !strings.Contains(err.Error(), "skep_api could not be started again") ||
		!strings.Contains(err.Error(), "service skep_api start") {
		t.Fatalf("err %v", err)
	}
}

func TestUpgradeCancelledWaitRestartsTheAPI(t *testing.T) {
	u, h := newUpgrade(t)
	ctx, cancel := context.WithCancel(context.Background())
	h.on["service skep_api stop"] = func() { setTasks(h.root, "running") }
	u.Sleep = func(time.Duration) { cancel() }
	err := u.Upgrade(ctx)
	if err == nil || !strings.Contains(err.Error(), "context canceled") || !strings.Contains(err.Error(), "skep_api was started again") {
		t.Fatalf("err %v", err)
	}
	apiRestarted(t, h)
}

func TestUpgradeBackupFailureRestartsTheAPI(t *testing.T) {
	u, h := newUpgrade(t)
	h.fail["skep-api db backup"] = errors.New("disk full")
	err := u.Upgrade(context.Background())
	if err == nil || !strings.Contains(err.Error(), "backup failed: disk full") || !strings.Contains(err.Error(), "skep_api was started again") {
		t.Fatalf("err %v", err)
	}
	if got := strings.Join(serviceCalls(h), ", "); got != "service skep_api status, service skep_api stop, service skep_api start" {
		t.Fatalf("services: %s", got)
	}
	apiRestarted(t, h)
}

func TestUpgradeVerifiesTheRunningFile(t *testing.T) {
	u, h := newUpgrade(t)
	h.textInode["skep_agent"] = "99" // still the old, replaced file
	err := u.Upgrade(context.Background())
	if err == nil || !strings.Contains(err.Error(), "runs inode 99, not the installed") ||
		!strings.Contains(err.Error(), "skep_agent was stopped and did not come back as the new build") {
		t.Fatalf("err %v", err)
	}
	// Every failure after the API stop starts it again, once.
	if got := strings.Join(serviceCalls(h), ", "); got != "service skep_api status, service skep_api stop, service skep_agent status, service skep_agent stop, service skep_agent start, service skep_api start" {
		t.Fatalf("services: %s", got)
	}
}

func TestUpgradeNoPIDFile(t *testing.T) {
	u, h := newUpgrade(t)
	h.noPIDFile["skep_agent"] = true
	if err := u.Upgrade(context.Background()); err == nil || !strings.Contains(err.Error(), "wrote no pid file") {
		t.Fatalf("err %v", err)
	}
}

func TestUpgradeUnhealthyAfterRestart(t *testing.T) {
	u, _ := newUpgrade(t)
	n := 0
	u.Agent = seqAgent{builds: []string{"0.1.0+dd7a8b6", "0.1.1+abc1234"}, n: &n, health: "degraded"}
	if err := u.Upgrade(context.Background()); err == nil || !strings.Contains(err.Error(), "health is degraded") {
		t.Fatalf("err %v", err)
	}
}

// A degraded health right after the first apply is usually the modules
// that load only at the next boot: the error names them and both fixes.
func TestUpgradeUnhealthyNamesModulesNotLoaded(t *testing.T) {
	u, _ := newUpgrade(t)
	n := 0
	u.Agent = seqAgent{builds: []string{"0.1.0+dd7a8b6", "0.1.1+abc1234"}, n: &n, health: "degraded"}
	appendFile(t, hostPath(u.Root, "/boot/loader.conf"), "vmm_load=\"YES\"\n")
	appendFile(t, hostPath(u.Root, "/etc/rc.conf"), "kld_list=\"if_epair netgraph\"\n")
	f := facts()
	f.Modules = loaded("if_epair")
	u.Prober = fakeProber{f}
	err := u.Upgrade(context.Background())
	if err == nil || !strings.Contains(err.Error(), "health is degraded") ||
		!strings.Contains(err.Error(), "Check: configured but not loaded: vmm netgraph (A reboot is the complete fix. kldload loads modules only: loader tunables added since boot need the reboot. To load the rest now: kldload -n vmm netgraph); skep-setup check") {
		t.Fatalf("err %v", err)
	}
	var inc *UpgradeIncomplete
	if !errors.As(err, &inc) {
		t.Fatalf("not incomplete: %T %v", err, err)
	}
}

// The cases in which the services were restarted but could not be
// verified are UpgradeIncomplete (skep-setup exits 4), say what to check,
// and say that running upgrade again is safe.
func TestUpgradeIncomplete(t *testing.T) {
	check := func(t *testing.T, err error, want ...string) {
		t.Helper()
		var inc *UpgradeIncomplete
		if !errors.As(err, &inc) {
			t.Fatalf("not incomplete: %T %v", err, err)
		}
		msg := err.Error()
		want = append(want, "incomplete: ", "Check: ", "Running skep-setup upgrade again is safe")
		for _, w := range want {
			if !strings.Contains(msg, w) {
				t.Fatalf("missing %q in: %s", w, msg)
			}
		}
		var ref *UpgradeRefusal
		if errors.As(err, &ref) {
			t.Fatal("also a refusal")
		}
	}
	t.Run("socket does not answer", func(t *testing.T) {
		u, _ := newUpgrade(t)
		n := 0
		u.Agent = seqAgent{builds: []string{"0.1.0+dd7a8b6", ""}, n: &n, health: "ok"}
		check(t, u.Upgrade(context.Background()), "did not answer on its socket within 3m20s", "jail_stop_completing")
	})
	t.Run("health not ok", func(t *testing.T) {
		u, _ := newUpgrade(t)
		n := 0
		u.Agent = seqAgent{builds: []string{"0.1.0+dd7a8b6", "0.1.1+abc1234"}, n: &n, health: "degraded"}
		check(t, u.Upgrade(context.Background()), "health is degraded", "skep-setup check")
	})
	t.Run("health unknown", func(t *testing.T) {
		u, _ := newUpgrade(t)
		n := 0
		u.Agent = healthErrAgent{seqAgent{builds: []string{"0.1.0+dd7a8b6", "0.1.1+abc1234"}, n: &n}}
		check(t, u.Upgrade(context.Background()), "health is unknown")
	})
	t.Run("API does not start after the agent restart", func(t *testing.T) {
		u, h := newUpgrade(t)
		h.fail["service skep_api start"] = errors.New("exit status 1")
		check(t, u.Upgrade(context.Background()), "skep_agent and skep_cluster run 0.1.1+abc1234, but skep_api did not come up",
			"service skep_api start; upgrade need not run again")
	})
}

// The agent stopped and not coming back as the new build, because its
// start or its check failed, is incomplete (skep-setup exits 4): upgrade
// changed the host and could not verify the result. It still starts
// skep_api again, and its advice does not promise that running upgrade
// again fixes it. The agent failing to stop is incomplete too: the stop
// may have taken effect.
func TestUpgradeAgentNotBackIsIncomplete(t *testing.T) {
	incomplete := func(t *testing.T, err error, want ...string) {
		t.Helper()
		var inc *UpgradeIncomplete
		if !errors.As(err, &inc) {
			t.Fatalf("not incomplete: %T %v", err, err)
		}
		msg := err.Error()
		want = append(want, "incomplete: skep_agent was stopped and did not come back as the new build: ",
			"skep_api was started again; backups in /", "Check: the agent's log (/var/log/daemon.log)",
			"helps only once the cause is fixed")
		for _, w := range want {
			if !strings.Contains(msg, w) {
				t.Fatalf("missing %q in: %s", w, msg)
			}
		}
		if strings.Contains(msg, "Running skep-setup upgrade again is safe") {
			t.Fatalf("promises a safe rerun: %s", msg)
		}
		var ref *UpgradeRefusal
		if errors.As(err, &ref) {
			t.Fatal("also a refusal")
		}
	}
	t.Run("start fails", func(t *testing.T) {
		u, h := newUpgrade(t)
		h.fail["service skep_agent start"] = errors.New("exit status 1")
		incomplete(t, u.Upgrade(context.Background()), "start skep_agent: exit status 1")
		apiRestarted(t, h)
	})
	t.Run("check fails", func(t *testing.T) {
		u, h := newUpgrade(t)
		h.textInode["skep_agent"] = "99" // still the old, replaced file
		incomplete(t, u.Upgrade(context.Background()), "runs inode 99, not the installed")
		apiRestarted(t, h)
	})
	t.Run("API cannot be started again either", func(t *testing.T) {
		u, h := newUpgrade(t)
		h.fail["service skep_agent start"] = errors.New("exit status 1")
		h.fail["service skep_api start"] = errors.New("exit status 1")
		err := u.Upgrade(context.Background())
		var inc *UpgradeIncomplete
		if !errors.As(err, &inc) || !strings.Contains(err.Error(), "skep_api could not be started again") ||
			!strings.Contains(err.Error(), "service skep_api start") {
			t.Fatalf("err %T %v", err, err)
		}
	})
	t.Run("stop fails: may have changed, incomplete", func(t *testing.T) {
		u, h := newUpgrade(t)
		h.fail["service skep_agent stop"] = errors.New("exit status 1")
		err := u.Upgrade(context.Background())
		var inc *UpgradeIncomplete
		if !errors.As(err, &inc) || !strings.HasPrefix(err.Error(), "incomplete: ") ||
			!strings.Contains(err.Error(), "stop skep_agent: ") || !strings.Contains(err.Error(), "may have taken effect") ||
			!strings.Contains(err.Error(), "backups in ") {
			t.Fatalf("err %T %v", err, err)
		}
		var ref *UpgradeRefusal
		if errors.As(err, &ref) {
			t.Fatal("also a refusal")
		}
		apiRestarted(t, h)
	})
	t.Run("stop fails and the API does not come back", func(t *testing.T) {
		u, h := newUpgrade(t)
		h.fail["service skep_agent stop"] = errors.New("exit status 1")
		h.fail["service skep_api start"] = errors.New("exit status 1")
		err := u.Upgrade(context.Background())
		var inc *UpgradeIncomplete
		if !errors.As(err, &inc) || !strings.Contains(err.Error(), "skep_api could not be started again") {
			t.Fatalf("err %T %v", err, err)
		}
	})
}

type healthErrAgent struct{ seqAgent }

func (healthErrAgent) Health(context.Context) (string, error) {
	return "", errors.New("connection reset")
}

func TestUpgradeUnknownOldBuild(t *testing.T) {
	u, h := newUpgrade(t)
	n := 0
	u.Agent = seqAgent{builds: []string{"", "0.1.1+abc1234"}, n: &n, health: "ok"}
	if err := u.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(h.calls, "\n"), "/var/db/skep/backup/unknown-20261122T170000Z") {
		t.Fatalf("calls %v", h.calls)
	}
}

func TestSocketAgent(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "sk")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "a.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(agentproto.HeaderActorID) != "skep-setup" || r.URL.Path != agentproto.Prefix+"/health" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set(agentproto.HeaderAgentVersion, "0.1.0+dd7a8b6")
		w.Write([]byte(`{"status":"ok","checks":{}}`))
	})}
	go srv.Serve(ln)
	defer srv.Close()
	a := SocketAgent{Path: path}
	if b, err := a.Build(context.Background()); err != nil || b != "0.1.0+dd7a8b6" {
		t.Fatalf("build %q %v", b, err)
	}
	if s, err := a.Health(context.Background()); err != nil || s != "ok" {
		t.Fatalf("health %q %v", s, err)
	}
	if _, err := (SocketAgent{Path: filepath.Join(dir, "none.sock")}).Build(context.Background()); err == nil {
		t.Fatal("no socket, no error")
	}
}

// A1: a service that is not running is not stopped ("service stop" fails
// on it); upgrade goes on and starts both.
func TestUpgradeStoppedServicesAreNotStopped(t *testing.T) {
	u, h := newUpgrade(t)
	os.Remove(hostPath(h.root, agentPID))
	os.Remove(hostPath(h.root, apiPID))
	h.fail["service skep_api stop"] = errors.New("skep_api not running? (check /var/run/skep/api.pid)")
	h.fail["service skep_agent stop"] = errors.New("skep_agent not running?")
	var out strings.Builder
	u.Out = &out
	if err := u.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(serviceCalls(h), ", "); got != "service skep_api status, service skep_agent status, service skep_agent start, service skep_cluster status, service skep_cluster stop, service skep_cluster start, service skep_api start" {
		t.Fatalf("services: %s", got)
	}
	if !strings.Contains(out.String(), "skep_api is not running") || !strings.Contains(out.String(), "skep_agent is not running") {
		t.Fatalf("output:\n%s", out.String())
	}
}

// A1: once skep_agent is being stopped, Ctrl-C cancels nothing: the agent
// and the API are started and verified, and health is reported.
func TestUpgradeCtrlCAfterTheAgentStopIsIgnored(t *testing.T) {
	u, h := newUpgrade(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.on["service skep_agent stop"] = cancel
	if err := u.Upgrade(ctx); err != nil {
		t.Fatal(err)
	}
	for _, c := range h.calls {
		if strings.HasPrefix(c, "CANCELLED") {
			t.Fatalf("a command was cancelled: %v", h.calls)
		}
	}
	if got := strings.Join(serviceCalls(h), ", "); !strings.HasSuffix(got, "service skep_agent stop, service skep_agent start, service skep_cluster status, service skep_cluster stop, service skep_cluster start, service skep_api start") {
		t.Fatalf("services: %s", got)
	}
}

// A1: Ctrl-C before the agent's stop (here during the backups) does stop
// the upgrade: the agent is left alone and the API started again.
func TestUpgradeCtrlCBeforeTheAgentStopRestoresTheAPI(t *testing.T) {
	u, h := newUpgrade(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.on["skep-api db backup"] = cancel
	err := u.Upgrade(ctx)
	if err == nil || !strings.Contains(err.Error(), "context canceled") || !strings.Contains(err.Error(), "skep_api was started again") {
		t.Fatalf("err %v", err)
	}
	for _, c := range h.calls {
		if strings.Contains(c, "skep_agent stop") || strings.Contains(c, "skep_agent start") {
			t.Fatalf("touched the agent: %v", h.calls)
		}
	}
	apiRestarted(t, h)
}

// A failed skep_api stop is exit 4. The API that still runs is not started
// again, and the agent is not touched.
func TestUpgradeAPIStopFailedStillRunning(t *testing.T) {
	u, h := newUpgrade(t)
	h.fail["service skep_api stop"] = errors.New("exit status 1")
	err := u.Upgrade(context.Background())
	var inc *UpgradeIncomplete
	if !errors.As(err, &inc) {
		t.Fatalf("err %v", err)
	}
	for _, w := range []string{"the stop of skep_api failed and may have taken effect", "skep_api was running when checked, but a supervisor still holding the failed stop can stop it afterwards",
		"skep_agent was not restarted", "helps only once the cause is fixed"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("missing %q: %v", w, err)
		}
	}
	if got := strings.Join(serviceCalls(h), ", "); got != "service skep_api status, service skep_api stop, service skep_api status, service skep_api status" {
		t.Fatalf("services: %s", got)
	}
}

// A failed stop that did take effect gets the one existing start.
func TestUpgradeAPIStopFailedButStopped(t *testing.T) {
	u, h := newUpgrade(t)
	h.fail["service skep_api stop"] = errors.New("exit status 1")
	h.on["service skep_api stop"] = func() { os.Remove(hostPath(h.root, apiPID)) }
	err := u.Upgrade(context.Background())
	var inc *UpgradeIncomplete
	if !errors.As(err, &inc) || !strings.Contains(err.Error(), "skep_api was running when checked, but a supervisor still holding the failed stop can stop it afterwards") || !strings.Contains(err.Error(), "skep_api was started again") {
		t.Fatalf("err %v", err)
	}
	if n := strings.Count(strings.Join(serviceCalls(h), ","), "service skep_api start"); n != 1 {
		t.Fatalf("%d starts: %v", n, serviceCalls(h))
	}
}

// A killed stop says "may have taken effect" once.
func TestUpgradeKilledStopSaysItOnce(t *testing.T) {
	killedErr := exec.Command("/bin/sh", "-c", "kill -9 $$").Run()
	for _, svc := range []string{"skep_api", "skep_agent"} {
		u, h := newUpgrade(t)
		h.fail["service "+svc+" stop"] = killedErr
		err := u.Upgrade(context.Background())
		var inc *UpgradeIncomplete
		if !errors.As(err, &inc) || strings.Count(err.Error(), "may have taken effect") != 1 {
			t.Errorf("%s: %v", svc, err)
		}
	}
}

// A1: the advice after a failed agent start does not promise that running
// upgrade again fixes it.
func TestUpgradeAgentStartFailureAdvice(t *testing.T) {
	u, h := newUpgrade(t)
	h.fail["service skep_agent start"] = errors.New("exit status 1")
	err := u.Upgrade(context.Background())
	if err == nil || !strings.Contains(err.Error(), "helps only once the cause is fixed") || !strings.Contains(err.Error(), "backups in /") {
		t.Fatalf("err %v", err)
	}
	apiRestarted(t, h)
}

// A2: -keep and the backup directory are checked before skep_api stops.
func TestUpgradeChecksKeepAndBackupDirFirst(t *testing.T) {
	t.Run("keep", func(t *testing.T) {
		u, h := newUpgrade(t)
		u.Keep = -1
		err := u.Upgrade(context.Background())
		var r *UpgradeRefusal
		if !errors.As(err, &r) || !strings.Contains(err.Error(), "at least one backup") || len(serviceCalls(h)) != 0 {
			t.Fatalf("err %v, services %v", err, serviceCalls(h))
		}
	})
	t.Run("backup parent", func(t *testing.T) {
		u, h := newUpgrade(t)
		os.WriteFile(hostPath(h.root, "/var/db/skep/backup"), []byte("a file"), 0o600)
		err := u.Upgrade(context.Background())
		var r *UpgradeRefusal
		if !errors.As(err, &r) || !strings.Contains(err.Error(), "cannot be created") || !strings.HasSuffix(err.Error(), "nothing was changed") {
			t.Fatalf("err %v", err)
		}
		if len(serviceCalls(h)) != 0 {
			t.Fatalf("services %v", serviceCalls(h))
		}
	})
}

// A3: after the starts, upgrade polls the agent's socket until it answers
// and requires the new build before it reports health.
func TestUpgradeWaitsForTheAgentSocket(t *testing.T) {
	t.Run("answers after two seconds", func(t *testing.T) {
		u, _ := newUpgrade(t)
		n := 0
		u.Agent = seqAgent{builds: []string{"0.1.0+dd7a8b6", "", "", "0.1.1+abc1234"}, n: &n, health: "ok"}
		var out strings.Builder
		u.Out = &out
		if err := u.Upgrade(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n != 4 || !strings.Contains(out.String(), "agent socket: answers as 0.1.1+abc1234") {
			t.Fatalf("asked %d times; output:\n%s", n, out.String())
		}
	})
	t.Run("old build on the socket", func(t *testing.T) {
		u, _ := newUpgrade(t)
		n := 0
		u.Agent = seqAgent{builds: []string{"0.1.0+dd7a8b6"}, n: &n, health: "ok"}
		err := u.Upgrade(context.Background())
		if err == nil || !strings.Contains(err.Error(), "reports build 0.1.0+dd7a8b6, want 0.1.1+abc1234") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("no answer", func(t *testing.T) {
		u, _ := newUpgrade(t)
		n := 0
		u.Agent = seqAgent{builds: []string{"0.1.0+dd7a8b6", ""}, n: &n, health: "ok"}
		u.SocketWait = 5 * time.Second
		err := u.Upgrade(context.Background())
		if err == nil || !strings.Contains(err.Error(), "did not answer on its socket within 5s") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("slow agent: one notice after 30 s, answer within the default wait", func(t *testing.T) {
		u, _ := newUpgrade(t)
		var out bytes.Buffer
		u.Out = &out
		// The old build, then no answer for 160 polls (160 s of 1 s
		// sleeps, an agent completing an interrupted jail stop), then the
		// new build.
		builds := []string{"0.1.0+dd7a8b6"}
		for i := 0; i < 160; i++ {
			builds = append(builds, "")
		}
		builds = append(builds, "0.1.1+abc1234")
		n := 0
		u.Agent = seqAgent{builds: builds, n: &n, health: "ok"}
		if err := u.Upgrade(context.Background()); err != nil {
			t.Fatalf("err %v; output:\n%s", err, out.String())
		}
		if c := strings.Count(out.String(), "it may be completing interrupted jail stops"); c != 1 {
			t.Fatalf("notice printed %d times; output:\n%s", c, out.String())
		}
		if !strings.Contains(out.String(), "waiting up to 3m20s") || !strings.Contains(out.String(), "agent socket: answers as 0.1.1+abc1234") {
			t.Fatalf("output:\n%s", out.String())
		}
	})
	t.Run("no answer within the default wait", func(t *testing.T) {
		u, _ := newUpgrade(t)
		var out bytes.Buffer
		u.Out = &out
		n := 0
		u.Agent = seqAgent{builds: []string{"0.1.0+dd7a8b6", ""}, n: &n, health: "ok"}
		err := u.Upgrade(context.Background())
		if err == nil || !strings.Contains(err.Error(), "did not answer on its socket within 3m20s") {
			t.Fatalf("err %v", err)
		}
		if c := strings.Count(out.String(), "it may be completing interrupted jail stops"); c != 1 {
			t.Fatalf("notice printed %d times; output:\n%s", c, out.String())
		}
	})
	t.Run("short wait: no notice", func(t *testing.T) {
		u, _ := newUpgrade(t)
		var out bytes.Buffer
		u.Out = &out
		n := 0
		u.Agent = seqAgent{builds: []string{"0.1.0+dd7a8b6", ""}, n: &n, health: "ok"}
		u.SocketWait = 30 * time.Second
		_ = u.Upgrade(context.Background())
		if strings.Contains(out.String(), "it may be completing interrupted jail stops") {
			t.Fatalf("notice with a 30 s wait; output:\n%s", out.String())
		}
	})
}

// A6: the database is the one the agent's configuration names.
func TestUpgradeReadsTheConfiguredDatabase(t *testing.T) {
	u, h := newUpgrade(t)
	os.MkdirAll(hostPath(h.root, "/usr/local/etc/skep"), 0o755)
	os.WriteFile(hostPath(h.root, agentJSONPath), []byte(`{"database":{"path":"/srv/skep/agent.db"}}`), 0o600)
	db, err := database.Open(context.Background(), hostPath(h.root, "/srv/skep/agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE TABLE tasks (id TEXT, status TEXT)`)
	db.Exec(`INSERT INTO tasks VALUES ('t1', 'running')`)
	db.Close()
	err = u.Upgrade(context.Background())
	if err == nil || !strings.Contains(err.Error(), "0 task(s) pending and 1 running") {
		t.Fatalf("err %v: the configured database was not read", err)
	}
}

// A6: an agent that answers on its socket has a database; if the
// configured one is missing, setup is reading the wrong configuration.
func TestUpgradeRefusesAnAnsweringAgentWithoutItsDatabase(t *testing.T) {
	u, h := newUpgrade(t)
	os.RemoveAll(hostPath(h.root, "/var/db/skep/agent"))
	err := u.Upgrade(context.Background())
	var r *UpgradeRefusal
	if !errors.As(err, &r) || !strings.Contains(err.Error(), "answers on its socket, but the database") || len(serviceCalls(h)) != 0 {
		t.Fatalf("err %v, services %v", err, serviceCalls(h))
	}
	// An agent that does not answer and has no database never started.
	u, h = newUpgrade(t)
	os.RemoveAll(hostPath(h.root, "/var/db/skep/agent"))
	u.Agent = startedAgent{h}
	if err := u.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// startedAgent answers, as the new build, only once skep_agent has been
// started.
type startedAgent struct{ h *upgradeHost }

func (a startedAgent) up() bool { return slices.Contains(a.h.calls, "service skep_agent start") }

func (a startedAgent) Build(context.Context) (string, error) {
	if !a.up() {
		return "", errors.New("connection refused")
	}
	return "0.1.1+abc1234", nil
}

func (a startedAgent) Health(context.Context) (string, error) {
	if !a.up() {
		return "", errors.New("connection refused")
	}
	return "ok", nil
}

// A6: a database without a tasks table is an error, not "no tasks".
func TestUpgradeMissingTasksTableIsAnError(t *testing.T) {
	u, h := newUpgrade(t)
	db, err := database.Open(context.Background(), hostPath(h.root, testAgentDB))
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`DROP TABLE tasks`)
	db.Close()
	err = u.Upgrade(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no such table") || len(serviceCalls(h)) != 0 {
		t.Fatalf("err %v, services %v", err, serviceCalls(h))
	}
}

// Part 0 b: a non-default data directory in rc.conf is refused before
// anything is stopped, naming the manual upgrade procedure; the default
// spelled differently is not.
func TestUpgradeRefusesANonDefaultDataDir(t *testing.T) {
	for _, line := range []string{`skep_agent_data_dir="/srv/skep"`, `skep_api_data_dir="/srv/skep"`} {
		u, h := newUpgrade(t)
		os.MkdirAll(hostPath(h.root, "/etc"), 0o755)
		os.WriteFile(hostPath(h.root, "/etc/rc.conf"), []byte(line+"\n"), 0o644)
		err := u.Upgrade(context.Background())
		var r *UpgradeRefusal
		if !errors.As(err, &r) || !strings.Contains(err.Error(), "https://github.com/greenbeanorg/skep/blob/main/docs/installation/upgrading.md") ||
			!strings.Contains(err.Error(), "nothing was changed") || len(serviceCalls(h)) != 0 {
			t.Fatalf("%s: err %v, services %v", line, err, serviceCalls(h))
		}
	}
	u, h := newUpgrade(t)
	os.MkdirAll(hostPath(h.root, "/etc"), 0o755)
	os.WriteFile(hostPath(h.root, "/etc/rc.conf"), []byte("skep_agent_data_dir=\"/var/db/skep/\"\n"), 0o644)
	if err := u.Upgrade(context.Background()); err != nil {
		t.Fatal(err)
	}
}
