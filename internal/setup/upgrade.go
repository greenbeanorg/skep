package setup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/dbbackup"
	"github.com/greenbeanorg/skep/pkg/version"

	_ "modernc.org/sqlite" // read-only look at the agent's tasks
)

// Installed programs and run files (the package layout).
const (
	agentBin    = "/usr/local/sbin/skep-agent"
	apiBin      = "/usr/local/sbin/skep-api"
	agentPID    = "/var/run/skep/agent.pid"
	apiPID      = "/var/run/skep/api.pid"
	agentSocket = "/var/run/skep/agent.sock"
	backupDir   = "/var/db/skep/backup"
)

// setupActor is the actor setup names on the agent's socket.
const setupActor = "skep-setup"

// Agent is what upgrade asks the running agent.
type Agent interface {
	// Build returns the running agent's build ID.
	Build(ctx context.Context) (string, error)
	// Health returns the agent's health status ("ok", "degraded",
	// "unavailable").
	Health(ctx context.Context) (string, error)
}

// SocketAgent talks to the agent's internal API on its Unix socket.
type SocketAgent struct{ Path string }

func (s SocketAgent) get(ctx context.Context, path string) (*http.Response, error) {
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", s.Path)
		}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent"+agentproto.Prefix+path, nil)
	if err != nil {
		return nil, err
	}
	agentproto.Caller{ActorID: setupActor, ActorName: setupActor}.SetHeaders(req.Header)
	return hc.Do(req)
}

// Build implements Agent.
func (s SocketAgent) Build(ctx context.Context) (string, error) {
	resp, err := s.get(ctx, "/health")
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	b := resp.Header.Get(agentproto.HeaderAgentVersion)
	if b == "" {
		return "", errors.New("the agent does not report its build (older than 0.1.0)")
	}
	return b, nil
}

// Health implements Agent.
func (s SocketAgent) Health(ctx context.Context) (string, error) {
	resp, err := s.get(ctx, "/health")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var h struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&h); err != nil {
		return "", err
	}
	return h.Status, nil
}

// Upgrader is skep-setup upgrade: after "pkg upgrade skep" replaced the
// files, it checks the new programs, backs up both databases, and
// restarts the services in order (ADR 0012 Decision 5).
type Upgrader struct {
	Root   string
	Runner Runner
	Prober Prober
	Agent  Agent
	Now    func() time.Time
	Out    io.Writer
	// Self is setup's own build; the installed programs must match it.
	Self version.Info
	// Force upgrades although tasks are pending or running: they become
	// interrupted and must be resubmitted.
	Force bool
	// Wait is how long, after skep_api is stopped, upgrade waits for
	// pending and running tasks to finish (default DefaultTaskWait).
	// Negative means do not wait.
	Wait time.Duration
	// Keep is how many database backups are kept: at least 1; 0 means
	// dbbackup.DefaultKeep.
	Keep int
	// Sleep and PIDWait pace the wait for a restarted service, SocketWait
	// the wait for the restarted agent to answer on its socket (default
	// DefaultSocketWait); Inode reads a file's inode. Tests replace them.
	Sleep      func(time.Duration)
	PIDWait    time.Duration
	SocketWait time.Duration
	Inode      func(path string) (uint64, error)

	// dbPath is the agent's database, from its configuration.
	dbPath string
}

// DefaultSocketWait is how long upgrade waits for the restarted agent to
// answer on its socket.
const DefaultSocketWait = 30 * time.Second

// DefaultTaskWait is how long upgrade waits for tasks after stopping
// skep_api.
const DefaultTaskWait = 60 * time.Second

// forceHint is what --force does to tasks, in every message about them.
const forceHint = "or use --force (pending and running tasks are then interrupted and must be resubmitted)"

// UpgradeRefusal is a reason upgrade did not upgrade. Before skep_api is
// stopped nothing has changed; after, skep_api has been started again and
// After says so.
type UpgradeRefusal struct {
	Reason string
	After  string
}

func (r *UpgradeRefusal) Error() string {
	if r.After != "" {
		return "refused: " + r.Reason + "; " + r.After
	}
	return "refused: " + r.Reason + "; nothing was changed"
}

// What a failure leaves, for restoreAPI's message.
const (
	// stateAgentUntouched: before skep_agent was stopped. The API is the
	// new build, the agent the old one.
	stateAgentUntouched = "skep_agent was not restarted, so skep_api (the new build) and skep-agent (the old one) " +
		"differ and the API answers 503 agent_version_mismatch until skep-setup upgrade completes; " +
		"run skep-setup upgrade again"
	// stateAPIStopFailed: "service skep_api stop" failed. Running upgrade
	// again would fail the same way until the cause is found.
	stateAPIStopFailed = "skep_agent was not restarted; find out why skep_api could not be stopped " +
		"(service skep_api status, and the API's log in /var/log/daemon.log), then run skep-setup upgrade again"
	// stateAgentFailed: skep_agent was stopped and did not come back as
	// the new build. Upgrade again repeats the same start, so it is no
	// remedy by itself.
	stateAgentFailed = "skep_agent did not come back as the new build, so the API cannot serve agent requests. " +
		"See the agent's log (/var/log/daemon.log): running skep-setup upgrade again repeats the same start " +
		"and helps only once the cause is fixed (it starts a stopped agent)"
)

func (u *Upgrader) printf(format string, args ...any) {
	if u.Out != nil {
		fmt.Fprintf(u.Out, format, args...)
	}
}

var versionLineRE = regexp.MustCompile(`^(skep-agent|skep-api) (\S+) \(commit (\S+),`)

// installedBuild runs "<bin> -version" and returns version+commit.
func (u *Upgrader) installedBuild(ctx context.Context, bin string) (string, error) {
	out, err := u.Runner.Run(ctx, Cmd{Name: hostPath(u.Root, bin), Args: []string{"-version"}})
	if err != nil {
		return "", err
	}
	m := versionLineRE.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return "", fmt.Errorf("%s -version printed %q", bin, strings.TrimSpace(out))
	}
	return m[2] + "+" + m[3], nil
}

// taskCount is the agent's unfinished tasks.
type taskCount struct{ pending, running int }

func (c taskCount) total() int { return c.pending + c.running }

func (c taskCount) String() string {
	return fmt.Sprintf("%d task(s) pending and %d running", c.pending, c.running)
}

// errAgentWithoutDB: the agent answers on its socket, but the database
// its configuration names does not exist, so setup is not reading what the
// agent uses.
type errAgentWithoutDB struct{ path string }

func (e *errAgentWithoutDB) Error() string {
	return fmt.Sprintf("the agent answers on its socket, but the database its configuration names (%s) does not exist: "+
		"setup is not reading what the agent uses; check skep_agent_config, skep_agent_data_dir and agent.json", e.path)
}

// agentWithoutDB is called when the agent's database does not exist: an
// agent that never started has none, but one that answers on its socket
// has one somewhere else.
func agentWithoutDB(ctx context.Context, ag Agent, root, path string) error {
	if ag == nil {
		return nil
	}
	if _, err := ag.Health(ctx); err == nil {
		return &errAgentWithoutDB{path: display(root, path)}
	}
	return nil
}

// display is a host path under root as the operator knows it.
func display(root, path string) string {
	if root == "/" || root == "" {
		return path
	}
	return "/" + strings.TrimPrefix(strings.TrimPrefix(path, strings.TrimSuffix(root, "/")), "/")
}

// openReadOnly opens a SQLite database read-only.
func openReadOnly(path string) (*sql.DB, error) {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	return sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?"+q.Encode())
}

// unfinishedTasks counts the agent's pending and running tasks,
// read-only, in a database that exists.
func unfinishedTasks(ctx context.Context, path string) (taskCount, error) {
	var c taskCount
	db, err := openReadOnly(path)
	if err != nil {
		return c, err
	}
	defer db.Close()
	err = db.QueryRowContext(ctx,
		`SELECT count(*) FILTER (WHERE status = 'pending'), count(*) FILTER (WHERE status = 'running')
		 FROM tasks WHERE status IN ('pending', 'running')`).Scan(&c.pending, &c.running)
	return c, err
}

// tasks reads the agent's unfinished tasks. A database that does not
// exist has none, unless the agent answers on its socket.
func (u *Upgrader) tasks(ctx context.Context) (taskCount, error) {
	if !exists(u.dbPath) {
		return taskCount{}, agentWithoutDB(ctx, u.Agent, u.Root, u.dbPath)
	}
	return unfinishedTasks(ctx, u.dbPath)
}

var labelSafeRE = regexp.MustCompile(`[^A-Za-z0-9._+-]`)

// Upgrade runs the steps; any refusal comes before anything changes.
func (u *Upgrader) Upgrade(ctx context.Context) error {
	if u.Keep == 0 {
		u.Keep = dbbackup.DefaultKeep
	}
	if u.Keep < 1 {
		return &UpgradeRefusal{Reason: fmt.Sprintf("keep %d: at least one backup must be kept", u.Keep)}
	}
	if u.SocketWait <= 0 {
		u.SocketWait = DefaultSocketWait
	}
	if u.Sleep == nil {
		u.Sleep = time.Sleep
	}
	if u.PIDWait == 0 {
		u.PIDWait = 20 * time.Second
	}
	if u.Inode == nil {
		u.Inode = inode
	}
	if u.Wait == 0 {
		u.Wait = DefaultTaskWait
	}
	// 1. Platform and versions.
	f, err := u.Prober.Probe(ctx)
	if err != nil {
		return err
	}
	if it := platformItem(f); it.Kind == Refuse {
		return &UpgradeRefusal{Reason: it.What + ": " + it.Detail}
	}
	want := u.Self.Version + "+" + u.Self.Commit
	for _, bin := range []string{agentBin, apiBin} {
		got, err := u.installedBuild(ctx, bin)
		if err != nil {
			return &UpgradeRefusal{Reason: err.Error()}
		}
		if got != want {
			return &UpgradeRefusal{Reason: fmt.Sprintf("%s is %s, skep-setup is %s: the package was not upgraded as one", bin, got, want)}
		}
	}
	u.printf("  installed: skep-agent, skep-api and skep-setup are %s\n", want)
	// 2. Unfinished tasks, while the API still accepts requests: the
	// common refusal, with nothing changed.
	if u.dbPath, err = AgentDBPath(u.Root); err != nil {
		return &UpgradeRefusal{Reason: "the agent's configuration: " + err.Error()}
	}
	tc, err := u.tasks(ctx)
	var noDB *errAgentWithoutDB
	if errors.As(err, &noDB) {
		return &UpgradeRefusal{Reason: noDB.Error()}
	}
	if err != nil {
		return fmt.Errorf("read the agent's tasks: %w", err)
	}
	if tc.total() > 0 && !u.Force {
		return &UpgradeRefusal{Reason: tc.String() + "; wait for them, " + forceHint}
	}
	// The backups' parent must be usable before anything is stopped.
	parent := hostPath(u.Root, backupDir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return &UpgradeRefusal{Reason: "the backup directory " + backupDir + " cannot be created: " + err.Error()}
	}
	if st, err := os.Lstat(parent); err != nil || !st.IsDir() {
		return &UpgradeRefusal{Reason: backupDir + " is not a directory"}
	}
	old, err := u.Agent.Build(ctx)
	if err != nil {
		u.printf("  the running agent's build is unknown (%v)\n", err)
		old = "unknown"
	}
	// 3. Stop the API, so no new task can be submitted, and look again.
	// From here on, every refusal or failure starts skep_api again
	// (restoreAPI), except a failure of its own final start.
	u.printf("  service skep_api stop\n")
	if err := stopService(ctx, u.Runner, "skep_api", u.printf); err != nil {
		return u.restoreAPI(ctx, want, fmt.Errorf("stop skep_api: %w", err), "no database was backed up; "+stateAPIStopFailed)
	}
	tc, err = u.waitForTasks(ctx)
	if err != nil {
		return u.restoreAPI(ctx, want, fmt.Errorf("read the agent's tasks after stopping skep_api: %w", err), "no database was backed up; "+stateAgentUntouched)
	}
	if tc.total() > 0 {
		if !u.Force {
			return u.restoreAPI(ctx, want, &UpgradeRefusal{
				Reason: fmt.Sprintf("%s after waiting %s with skep_api stopped; wait for them, ", tc, max(u.Wait, 0)) + forceHint},
				"no database was backed up; "+stateAgentUntouched+" once the tasks have finished")
		}
		u.printf("  --force: %s are interrupted by the agent restart and must be resubmitted\n", tc)
	}
	// 4. Backups, labelled with the running (old) build.
	label := labelSafeRE.ReplaceAllString(old, "_")
	dir := parent + "/" + label + "-" + u.Now().UTC().Format("20060102T150405Z")
	for _, bin := range []string{agentBin, apiBin} {
		out, err := u.Runner.Run(ctx, Cmd{Name: hostPath(u.Root, bin), Args: []string{"db", "backup", "-dir", dir, "-keep", strconv.Itoa(u.Keep)}})
		if err != nil {
			return u.restoreAPI(ctx, want, fmt.Errorf("backup failed: %w", err), stateAgentUntouched)
		}
		u.printf("%s", indent(out))
	}
	// 5. Restart the agent, then the API, each verified. From the agent's
	// stop on, Ctrl-C (or SIGTERM) no longer cancels anything: stopping
	// half way would leave the host without an agent. The commands run in
	// their own process group, so the terminal's signal does not reach
	// them either; every wait below is bounded.
	if err := ctx.Err(); err != nil {
		return u.restoreAPI(ctx, want, err, "backups in "+dir+"; "+stateAgentUntouched)
	}
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*(u.PIDWait+time.Minute)+u.SocketWait+time.Minute)
	defer cancel()
	u.printf("  service skep_agent stop (from here on, Ctrl-C does not interrupt the upgrade)\n")
	if err := stopService(nctx, u.Runner, "skep_agent", u.printf); err != nil {
		return u.restoreAPI(nctx, want, fmt.Errorf("stop skep_agent: %w", err), "backups in "+dir+"; "+stateAgentFailed)
	}
	if _, err := u.Runner.Run(nctx, Cmd{Name: cmdService, Args: []string{"skep_agent", "start"}}); err != nil {
		return u.restoreAPI(nctx, want, fmt.Errorf("start skep_agent: %w", err), "backups in "+dir+"; "+stateAgentFailed)
	}
	if err := u.verify(nctx, "skep_agent", agentBin, agentPID, want); err != nil {
		return u.restoreAPI(nctx, want, err, "backups in "+dir+"; "+stateAgentFailed)
	}
	apiDown := func(err error) error {
		return fmt.Errorf("skep_agent runs %s, but skep_api did not come up: %w; see the API's log (/var/log/daemon.log) "+
			"and start it with service skep_api start; upgrade need not run again (backups in %s)", want, err, dir)
	}
	if _, err := u.Runner.Run(nctx, Cmd{Name: cmdService, Args: []string{"skep_api", "start"}}); err != nil {
		return apiDown(err)
	}
	if err := u.verify(nctx, "skep_api", apiBin, apiPID, want); err != nil {
		return apiDown(err)
	}
	// 6. The agent answers on its socket as the new build, then health.
	if err := u.waitForAgent(nctx, want); err != nil {
		return fmt.Errorf("both services restarted, but %w (backups in %s)", err, dir)
	}
	status, err := u.Agent.Health(nctx)
	if err != nil {
		return fmt.Errorf("both services restarted, but the agent's health is unknown: %w", err)
	}
	u.printf("  health: %s\n", status)
	if status != "ok" {
		return fmt.Errorf("both services restarted at %s, but health is %s; see skep-setup check and the agent's log", want, status)
	}
	u.printf("upgraded to %s; backups in %s\n", want, dir)
	return nil
}

// waitForAgent polls the agent's socket every second for up to
// u.SocketWait until it answers, and requires the build it reports in
// Skep-Agent-Version to be want.
func (u *Upgrader) waitForAgent(ctx context.Context, want string) error {
	var last error
	for waited := time.Duration(0); ; waited += time.Second {
		got, err := u.Agent.Build(ctx)
		if err == nil {
			if got != want {
				return fmt.Errorf("the agent on its socket reports build %s, want %s", got, want)
			}
			u.printf("  agent socket: answers as %s\n", got)
			return nil
		}
		last = err
		if waited >= u.SocketWait {
			return fmt.Errorf("the agent did not answer on its socket within %s: %v", u.SocketWait, last)
		}
		u.Sleep(time.Second)
	}
}

// waitForTasks re-reads the agent's unfinished tasks once skep_api is
// stopped, and polls every second for up to u.Wait while any remain.
func (u *Upgrader) waitForTasks(ctx context.Context) (taskCount, error) {
	tc, err := u.tasks(ctx)
	if err != nil || tc.total() == 0 || u.Wait <= 0 {
		return tc, err
	}
	u.printf("  %s; waiting up to %s for them to finish\n", tc, u.Wait)
	for waited := time.Duration(0); waited < u.Wait; waited += time.Second {
		if err := ctx.Err(); err != nil {
			return tc, err
		}
		u.Sleep(time.Second)
		if tc, err = u.tasks(ctx); err != nil || tc.total() == 0 {
			return tc, err
		}
	}
	return tc, nil
}

// restoreAPI starts skep_api again after cause, checks its pid file and
// that it runs the installed file, reports the agent's health, and
// returns cause with what happened to the API and state, what else the
// failure left.
func (u *Upgrader) restoreAPI(ctx context.Context, want string, cause error, state string) error {
	// The context may be what failed; starting the API must not depend
	// on it.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.PIDWait+30*time.Second)
	defer cancel()
	var problem error
	if _, err := u.Runner.Run(rctx, Cmd{Name: cmdService, Args: []string{"skep_api", "start"}}); err != nil {
		problem = fmt.Errorf("start skep_api: %w", err)
	} else if err := u.verify(rctx, "skep_api", apiBin, apiPID, want); err != nil {
		problem = err
	}
	if problem != nil {
		return fmt.Errorf("%w; and skep_api could not be started again: %v (start it by hand: service skep_api start); %s", cause, problem, state)
	}
	u.printf("  service skep_api start (after the failure)\n")
	if status, err := u.Agent.Health(rctx); err != nil {
		u.printf("  agent health: unknown (%v)\n", err)
	} else {
		u.printf("  agent health: %s\n", status)
	}
	after := "skep_api was started again; " + state
	var r *UpgradeRefusal
	if errors.As(cause, &r) {
		r.After = after
		return r
	}
	return fmt.Errorf("%w; %s", cause, after)
}

func indent(s string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if l != "" {
			b.WriteString("  " + l + "\n")
		}
	}
	return b.String()
}

var fstatTextRE = regexp.MustCompile(`(?m)^\S+\s+\S+\s+(\d+)\s+text\s+\S+\s+(\d+)\s`)

// verify waits for the service's pid file and checks the process runs the
// installed file (the inode of its text is the file's) at the new build.
func (u *Upgrader) verify(ctx context.Context, svc, bin, pidFile, want string) error {
	var pid string
	for waited := time.Duration(0); ; waited += time.Second {
		b, err := os.ReadFile(hostPath(u.Root, pidFile))
		if err == nil && strings.TrimSpace(string(b)) != "" {
			pid = strings.TrimSpace(string(b))
			break
		}
		if waited >= u.PIDWait {
			return fmt.Errorf("%s wrote no pid file in %s", svc, u.PIDWait)
		}
		u.Sleep(time.Second)
	}
	if _, err := strconv.Atoi(pid); err != nil {
		return fmt.Errorf("%s: pid file holds %q", svc, pid)
	}
	out, err := u.Runner.Run(ctx, Cmd{Name: cmdFstat, Args: []string{"-p", pid}})
	if err != nil {
		return fmt.Errorf("%s (pid %s) is not running: %w", svc, pid, err)
	}
	m := fstatTextRE.FindStringSubmatch(out)
	if m == nil || m[1] != pid {
		return fmt.Errorf("%s (pid %s): no text entry in fstat", svc, pid)
	}
	ino, err := u.Inode(hostPath(u.Root, bin))
	if err != nil {
		return err
	}
	if m[2] != strconv.FormatUint(ino, 10) {
		return fmt.Errorf("%s (pid %s) runs inode %s, not the installed %s (inode %d)", svc, pid, m[2], bin, ino)
	}
	got, err := u.installedBuild(ctx, bin)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s runs %s, want %s", svc, got, want)
	}
	u.printf("  %s pid %s runs %s at %s\n", svc, pid, bin, want)
	return nil
}

func inode(path string) (uint64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("no inode for " + path)
	}
	return uint64(sys.Ino), nil
}
