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
	"path"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/config"
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
	// Cluster runs skep-cluster's commands and Importer skep-api's import
	// commands; nil means the installed programs (HostCluster,
	// HostImporter). LeaderWait bounds the wait for a leader after
	// skep_cluster's start (default DefaultUpgradeLeaderWait), ImportWait
	// the wait for an import state after skep_api's (default 30 s).
	Cluster    Cluster
	Importer   Importer
	LeaderWait time.Duration
	ImportWait time.Duration

	// dbPath is the agent's database, from its configuration.
	dbPath string
}

// DefaultUpgradeLeaderWait is how long upgrade waits, after skep_cluster's
// start, for its status to name a leader (D11 step 6 of step 8c-2).
const DefaultUpgradeLeaderWait = 60 * time.Second

// loginsWithoutCluster is what works while skep_cluster is not running or
// has no leader (ADR 0013 amendment 4, D18 of 8c-1): the texts of D12.
const loginsWithoutCluster = "Meanwhile skep-api has no quorum: an enabled administrator can log in, with a degraded session of at most one hour; " +
	"every other user's login and every user change answer 503 no_quorum"

// DefaultSocketWait is how long upgrade waits for the restarted agent to
// answer on its socket. The agent serves its socket only after its
// start-up work, which includes completing jail stops an agent restart
// interrupted (ADR 0010 amendment aa): up to about 190 s for one jail with
// the default timeouts.
const DefaultSocketWait = 200 * time.Second

// socketNoticeAfter is when the wait for the agent's socket prints its one
// line saying why the agent may be slow.
const socketNoticeAfter = 30 * time.Second

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

// UpgradeIncomplete is an upgrade that made changes and could not verify
// the result: a service's stop failed and may have taken effect,
// skep_agent was stopped and did not come back as the new
// build (its start or its check failed), skep_api did not come up after
// the agent's restart, the agent did not answer on its socket within the
// wait, or its health was not ok. skep-setup exits 4 for it.
type UpgradeIncomplete struct {
	// Problem says what could not be verified; Check says what to look at.
	Problem, Check string
	// Again, when set, replaces the default advice on running upgrade
	// again (which assumes both services already run the new build).
	Again string
}

// againSafe is the advice for the cases in which both services already
// run the new build.
const againSafe = "Running skep-setup upgrade again is safe " +
	"(the services already run the new build; it backs up and restarts both again) but does not fix the cause, " +
	"and while the agent is still completing interrupted jail stops a second restart only delays them"

func (e *UpgradeIncomplete) Error() string {
	again := e.Again
	if again == "" {
		again = againSafe
	}
	return "incomplete: " + e.Problem + ". Check: " + e.Check + ". " + again
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
	// againAPIStopFailed: upgrade again repeats the stop that failed.
	againAPIStopFailed = "Running skep-setup upgrade again repeats the same stop and helps only once the cause is fixed"
	// againAgentFailed: skep_agent's stop, start or check failed, so it is
	// not known to run the new build. Upgrade again repeats the same
	// start, so it is no remedy by itself.
	againAgentFailed = "Until skep_agent runs the new build the API cannot serve agent requests; " +
		"running skep-setup upgrade again repeats the same start and helps only once the cause is fixed (it starts a stopped agent)"
)

func (u *Upgrader) printf(format string, args ...any) {
	if u.Out != nil {
		fmt.Fprintf(u.Out, format, args...)
	}
}

var versionLineRE = regexp.MustCompile(`^(skep-agent|skep-api) (\S+) \(commit (\S+),`)

// installedBuild runs "<bin> -version" and returns version+commit;
// skep-cluster prints its version with the "version" command, in its own
// shape (facts F3 of step 8c-2).
func (u *Upgrader) installedBuild(ctx context.Context, bin string) (string, error) {
	if bin == clusterBin {
		return u.cluster().Build(ctx)
	}
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
	if u.LeaderWait <= 0 {
		u.LeaderWait = DefaultUpgradeLeaderWait
	}
	if u.ImportWait <= 0 {
		u.ImportWait = importWait
	}
	// Setup backs the databases up under /var/db/skep: refuse a data
	// directory someone other than root can change.
	if err := CheckDataDir(u.Root); err != nil {
		return &UpgradeRefusal{Reason: err.Error()}
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
	cl := u.cluster()
	if got, err := cl.Build(ctx); err != nil {
		return &UpgradeRefusal{Reason: err.Error()}
	} else if got != want {
		return &UpgradeRefusal{Reason: fmt.Sprintf("%s is %s, skep-setup is %s: the package was not upgraded as one", clusterBin, Clean(got), want)}
	}
	u.printf("  installed: skep-agent, skep-api, skep-cluster and skep-setup are %s\n", want)
	// The backup directory, the backup commands and the checks below
	// assume the default data directory.
	if err := defaultDataDirs(u.Root); err != nil {
		return &UpgradeRefusal{Reason: err.Error()}
	}
	cs, err := u.clusterRefusals(ctx, f)
	if err != nil {
		return err
	}
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
		return u.apiStopFailed(ctx, want, err)
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
	u.printf("  the cluster store (%s) is not backed up by this version\n", clusterDataDir)
	// 5. Restart the agent, then the cluster, then the API, each
	// verified. From the agent's stop on, Ctrl-C (or SIGTERM) no longer
	// cancels anything: stopping half way would leave the host without an
	// agent. The commands run in their own process group, so the
	// terminal's signal does not reach them either; every wait below is
	// bounded, and the context's limit is their sum (V6 of step 8c-2).
	if err := ctx.Err(); err != nil {
		return u.restoreAPI(ctx, want, err, "backups in "+dir+"; "+stateAgentUntouched)
	}
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.uninterruptibleLimit())
	defer cancel()
	u.printf("  service skep_agent stop (from here on, Ctrl-C does not interrupt the upgrade)\n")
	if err := stopService(nctx, u.Runner, "skep_agent", u.printf); err != nil {
		// A failed stop may still have delivered its signal, so the agent
		// may be down: the same rule as apply and remove (exit 4).
		restored := u.restoreAPI(nctx, want, fmt.Errorf("stop skep_agent: %w", err), "backups in "+dir)
		return &UpgradeIncomplete{
			Problem: stopFailedText("skep_agent", err) + ", so the agent may be down or still the old build: " + restored.Error(),
			Check:   "service skep_agent status and the agent's log (/var/log/daemon.log)",
			Again:   againAgentFailed}
	}
	// The agent was stopped: from here on a failure has changed the host
	// and is incomplete (exit 4), not a refusal.
	agentDown := func(err error) error {
		restored := u.restoreAPI(nctx, want, err, "backups in "+dir)
		return &UpgradeIncomplete{
			Problem: "skep_agent was stopped and did not come back as the new build: " + restored.Error(),
			Check:   "the agent's log (/var/log/daemon.log), service skep_agent status and skep-setup check",
			Again:   againAgentFailed}
	}
	if _, err := u.Runner.Run(nctx, Cmd{Name: cmdService, Args: []string{"skep_agent", "start"}}); err != nil {
		return agentDown(fmt.Errorf("start skep_agent: %w", err))
	}
	if err := u.verify(nctx, "skep_agent", agentBin, agentPID, want); err != nil {
		return agentDown(err)
	}
	// clusterDown is a failure of steps 4 to 6 (D12): skep_api is started
	// again before it returns, and the text says what works meanwhile.
	clusterDown := func(problem, check, again string) error {
		restored := u.restoreAPI(nctx, want, errors.New(problem), "backups in "+dir)
		return &UpgradeIncomplete{Problem: restored.Error() + ". " + loginsWithoutCluster, Check: check, Again: again}
	}
	agentAnswered := false
	// 6. Only on a host without a cluster: wait for the agent's socket
	// (init reads the node ID from it), then "skep-cluster init".
	if !cs.initialised {
		if err := u.waitForAgent(nctx, want); err != nil {
			return clusterDown(fmt.Sprintf("skep_agent runs %s, but %v, so skep-cluster init could not run (backups in %s)", want, err, dir),
				"the agent's log (/var/log/daemon.log): jail_stop_completing means it is completing interrupted jail stops; then service skep_agent status",
				"Running skep-setup upgrade again is safe: it initialises the cluster once the agent answers")
		}
		agentAnswered = true
		u.printf("  skep-cluster init\n")
		if err := cl.Init(nctx); err != nil {
			return clusterDown(fmt.Sprintf("skep-cluster init failed and may have changed the host: %v (backups in %s)", err, dir),
				"skep-cluster init -check, and the output above; a half-made cluster store is removed by init itself",
				"Running skep-setup upgrade again is safe: it runs init again only while the host is not initialised")
		}
	}
	// 7. Only when skep_cluster_enable is unset: add it, with a journal
	// of command "upgrade" (the same re-read rule as apply).
	if cs.addEnable {
		if err := u.addClusterEnable(nctx); err != nil {
			return clusterDown(fmt.Sprintf("adding skep_cluster_enable=\"YES\" to %s failed: %v (backups in %s)", rcConfPath, err, dir),
				"the journal in /"+journalRoot+" (skep-setup rollback lists it) and "+rcConfPath,
				"Clear the journal with skep-setup rollback if it is unfinished, then run skep-setup upgrade again")
		}
	}
	// 8. Restart skep_cluster as the new build and wait for a leader.
	if err := u.restartCluster(nctx, want); err != nil {
		return clusterDown(fmt.Sprintf("skep_agent runs %s, but skep_cluster did not come up as it: %v (backups in %s)", want, err, dir),
			"service skep_cluster status, skep-cluster status, and skep-cluster's log (/var/log/daemon.log)",
			"Running skep-setup upgrade again is safe: it restarts skep_cluster again, but does not fix the cause")
	}
	apiDown := func(err error) error {
		return &UpgradeIncomplete{
			Problem: fmt.Sprintf("skep_agent and skep_cluster run %s, but skep_api did not come up: %v (backups in %s)", want, err, dir),
			Check:   "the API's log (/var/log/daemon.log), then start it with service skep_api start; upgrade need not run again"}
	}
	if _, err := u.Runner.Run(nctx, Cmd{Name: cmdService, Args: []string{"skep_api", "start"}}); err != nil {
		return apiDown(err)
	}
	if err := u.verify(nctx, "skep_api", apiBin, apiPID, want); err != nil {
		return apiDown(err)
	}
	// 9. The agent answers on its socket as the new build, then health.
	if !agentAnswered {
		if err := u.waitForAgent(nctx, want); err != nil {
			return &UpgradeIncomplete{
				Problem: fmt.Sprintf("the services restarted, but %v (backups in %s)", err, dir),
				Check: "the agent's log (/var/log/daemon.log): jail_stop_completing means it is completing interrupted jail " +
					"stops and will answer when they end; then service skep_agent status and skep-setup check"}
		}
	}
	status, err := u.Agent.Health(nctx)
	if err != nil {
		return &UpgradeIncomplete{
			Problem: fmt.Sprintf("the services restarted at %s, but the agent's health is unknown: %v (backups in %s)", want, err, dir),
			Check:   "the agent's log (/var/log/daemon.log) and skep-setup check"}
	}
	u.printf("  health: %s\n", status)
	if status != "ok" {
		return &UpgradeIncomplete{
			Problem: fmt.Sprintf("the services restarted at %s, but health is %s (backups in %s)", want, status, dir),
			Check:   u.modulesHint(nctx) + "skep-setup check and the agent's log (/var/log/daemon.log)"}
	}
	// 10. The import of the host's users (D13).
	if err := u.importUsers(nctx); err != nil {
		return err
	}
	u.printf("upgraded to %s; backups in %s\n", want, dir)
	return nil
}

// cluster is the Cluster upgrade asks.
func (u *Upgrader) cluster() Cluster {
	if u.Cluster != nil {
		return u.Cluster
	}
	return HostCluster{Root: u.Root, Runner: u.Runner}
}

// importer is the Importer upgrade asks.
func (u *Upgrader) importer() Importer {
	if u.Importer != nil {
		return u.Importer
	}
	return HostImporter{Root: u.Root, Runner: u.Runner}
}

// uninterruptibleLimit is the sum of every bounded wait from the agent's
// stop to the end (V6): the agent's stop and start with its pid wait, the
// agent's socket, init, the rc.conf line, skep_cluster's stop and start
// with its pid wait and the leader, skep_api's start with its pid wait,
// the import state and the import, and a minute to spare. Each command
// without a limit of its own is counted as one minute.
func (u *Upgrader) uninterruptibleLimit() time.Duration {
	return 3*(u.PIDWait+time.Minute) + u.SocketWait + clusterInitTimeout + time.Minute +
		time.Minute + u.LeaderWait + u.ImportWait + 2*importTimeout + 2*time.Minute
}

// clusterState is what upgrade found out about skep_cluster before
// anything was stopped.
type clusterState struct {
	initialised bool
	addEnable   bool
}

// clusterRefusals makes D10's checks, before anything is stopped.
func (u *Upgrader) clusterRefusals(ctx context.Context, f Facts) (clusterState, error) {
	var cs clusterState
	refuse := func(reason string) (clusterState, error) { return cs, &UpgradeRefusal{Reason: reason} }
	switch it := clusterAccountItem(u.Root); it.Kind {
	case Refuse:
		return refuse(it.What + ": " + it.Detail)
	case Warn:
		u.printf("  warning: %s: %s\n", it.What, it.Detail)
	}
	rc, err := ReadRCConf(u.Root)
	if err != nil {
		return refuse("rc.conf: " + err.Error())
	}
	if v, set := rc.Get("skep_cluster_enable"); set {
		if !strings.EqualFold(v, "YES") {
			return refuse(fmt.Sprintf("skep_cluster_enable is %q: skep-api needs skep_cluster from this build on; set it to YES, then run upgrade again", v))
		}
	} else {
		cs.addEnable = true
		if len(rc.Main.Complex) > 0 || rc.ComplexOverride() != "" {
			return refuse("skep_cluster_enable is not set, and setup cannot edit rc.conf by its rules (it is not plain assignments); " +
				"run by hand: sysrc skep_cluster_enable=YES, then run upgrade again")
		}
		journals, err := LoadJournals(u.Root)
		if err != nil {
			return refuse("the journals: " + err.Error())
		}
		if j := incompleteJournal(journals); j != nil {
			return refuse("skep_cluster_enable must be added to rc.conf, and an earlier run did not finish (journal " + j.ID + "): run skep-setup rollback " + j.ID + " first")
		}
	}
	for v, def := range map[string]string{"skep_cluster_data_dir": clusterDataDir, "skep_cluster_config": clusterJSONPath,
		"skep_cluster_socket_dir": "/var/run/skep-cluster", "skep_cluster_run_dir": "/var/run/skep"} {
		if d, set := rc.Get(v); set && path.Clean(d) != def {
			return refuse(fmt.Sprintf("%s is %q, not the default %s: skep-setup upgrade handles only the default layout; upgrade by hand as %s describes", v, d, def, manualUpgradeDoc))
		}
	}
	cl := u.cluster()
	if cs.initialised, err = cl.Initialised(ctx); err != nil {
		return refuse(err.Error())
	}
	if !f.Running["skep_cluster"] {
		if cs.initialised {
			u.printf("  skep_cluster is initialised but not running; upgrade starts it\n")
		} else {
			u.printf("  this host has no cluster yet; upgrade runs skep-cluster init (a one-node cluster, no port opened)\n")
		}
		return cs, nil
	}
	v, err := cl.Status(ctx)
	switch {
	case err != nil:
		return refuse("skep_cluster runs, but its status cannot be read (" + err.Error() + "), so upgrade cannot tell whether it is joining, rotating its key or removed; find out why (skep-cluster status), then run upgrade again")
	case v.Removed != nil:
		return refuse("this node was removed from cluster " + Clean(v.Removed.ClusterID) + ": run skep-cluster leave --wipe as root first (it makes this host a new one-node cluster)")
	case v.Join != nil:
		return refuse("this node is joining cluster " + Clean(v.Join.ClusterID) + " (phase " + Clean(v.Join.Phase) + "): let the join finish, or run skep-cluster join --abandon as root, then run upgrade again")
	case v.Rotation != nil:
		return refuse("this node is rotating its key (phase " + Clean(v.Rotation.Phase) + "): wait until skep-cluster status shows no rotation, or run skep-cluster key rotate --abort as root, then run upgrade again")
	}
	if v.StateMachine.Stalled != nil {
		u.printf("  skep-cluster's state machine is stopped at log index %d; the new build may apply it\n", v.StateMachine.Stalled.Index)
	}
	return cs, nil
}

// addClusterEnable adds skep_cluster_enable="YES" through the Applier,
// with a journal of command "upgrade", and checks the line afterwards: an
// action skipped because the line appeared meanwhile is fine only if it
// says YES.
func (u *Upgrader) addClusterEnable(ctx context.Context) error {
	a := &Applier{Root: u.Root, Runner: u.Runner, Now: u.Now, Out: u.Out, SkepVersion: u.Self.String(), Command: "upgrade"}
	act := Action{Type: ActSysrc, File: rcConfPath, Name: "skep_cluster_enable", Op: "=", Value: "YES"}
	p := Plan{Items: []Item{{Kind: Add, Area: rcConfPath, What: `skep_cluster_enable="YES"`, Action: &act}}}
	if _, err := a.Apply(ctx, p); err != nil {
		return err
	}
	rc, err := ReadRCConf(u.Root)
	if err != nil {
		return err
	}
	if v, _ := rc.Get("skep_cluster_enable"); !strings.EqualFold(v, "YES") {
		return fmt.Errorf("skep_cluster_enable is %q after the change", v)
	}
	return nil
}

// restartCluster stops skep_cluster if it runs, starts it, checks it runs
// the installed file at the new build, and waits for its status to show
// the new build, a state machine that is not stopped and a leader.
func (u *Upgrader) restartCluster(ctx context.Context, want string) error {
	u.printf("  service skep_cluster stop\n")
	if err := stopService(ctx, u.Runner, "skep_cluster", u.printf); err != nil {
		return fmt.Errorf("stop skep_cluster: %w", err)
	}
	if _, err := u.Runner.Run(ctx, Cmd{Name: cmdService, Args: []string{"skep_cluster", "start"}}); err != nil {
		return fmt.Errorf("start skep_cluster: %w", err)
	}
	if err := u.verify(ctx, "skep_cluster", clusterBin, clusterPID, want); err != nil {
		return err
	}
	v, err := waitForLeader(ctx, u.cluster(), u.LeaderWait, u.Sleep)
	if err != nil {
		return err
	}
	if v.Build != want {
		return fmt.Errorf("skep-cluster's status reports build %s, want %s", Clean(v.Build), want)
	}
	u.printf("  skep_cluster: %s\n", v.Summary())
	return nil
}

// importUsers is D13: read the import state, import alone, never with
// --yes, and print the cluster steps otherwise.
func (u *Upgrader) importUsers(ctx context.Context) error {
	im := u.importer()
	st, err := waitForImportState(ctx, im, u.ImportWait, u.Sleep)
	if err != nil {
		return &UpgradeIncomplete{Problem: "the services run the new build, but the state of the import of this host's users is unknown: " + err.Error(),
			Check: "skep-api admin list --import-state (as root)", Again: "Running skep-setup upgrade again is safe; it only reads the state again"}
	}
	if st != ImportPending {
		u.printf("  users: import state %s\n", st)
		return nil
	}
	v, err := u.cluster().Status(ctx)
	if err != nil {
		return &UpgradeIncomplete{Problem: "this host's users are still to be imported, and skep-cluster's status cannot be read: " + err.Error(),
			Check: "skep-cluster status", Again: "Running skep-setup upgrade again is safe; it imports once skep-cluster answers"}
	}
	if !v.Alone() {
		u.printf("  users: import state pending, and this node is not alone in its cluster: setup does not import here.\n" +
			"  Upgrade every node first, then run \"" + ImportCommand + "\" on one node only; on every other node run it, then again with --yes\n" +
			"  (docs/installation/upgrading.md). Until then logins work from this host's own users and user changes answer 503 users_import_pending.\n")
		return nil
	}
	u.printf("  users: import state pending; %s\n", ImportCommand)
	out, ierr := im.Import(ctx)
	if out != "" {
		u.printf("%s", indent(out))
	}
	after, serr := im.State(ctx)
	if ierr == nil && serr == nil && after == ImportDone {
		u.printf("  users: import state done\n")
		return nil
	}
	why := fmt.Sprintf("the state is %q afterwards", after)
	switch {
	case ierr != nil:
		why = ierr.Error()
	case serr != nil:
		why = "the state cannot be read afterwards: " + serr.Error()
	}
	return &UpgradeIncomplete{
		Problem: "the services run the new build, but the import of this host's users did not finish: " + why +
			". Meanwhile logins work from this host's own users and user changes answer 503 users_import_pending",
		Check: "skep-cluster status and skep-api admin list --import-state",
		Again: "Run \"" + ImportCommand + "\" as root (it finishes an interrupted import), or skep-setup upgrade again"}
}

// stopFailedText says a stop failed and may have taken effect, unless it
// was killed: that error's own message already says so.
func stopFailedText(svc string, err error) string {
	if strings.Contains(err.Error(), "may have taken effect") {
		return "the stop of " + svc + " failed"
	}
	return "the stop of " + svc + " failed and may have taken effect"
}

// apiStopFailed is a failed "service skep_api stop" (exit 4). A skep_api
// still running is left alone; one that is down gets restoreAPI's start.
func (u *Upgrader) apiStopFailed(ctx context.Context, want string, stopErr error) error {
	running := func() bool {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, err := u.Runner.Run(rctx, Cmd{Name: cmdService, Args: []string{"skep_api", "status"}})
		return err == nil
	}
	cause, state := fmt.Errorf("stop skep_api: %w", stopErr), "no database was backed up; "+stateAPIStopFailed
	detail := fmt.Sprintf("%v; %s", cause, state)
	if !running() {
		detail = u.restoreAPI(ctx, want, cause, state).Error()
	}
	// Only a reading: a supervisor still holding the failed stop can stop
	// a running skep_api afterwards (seen on langrishe, 2026-11-23).
	now := map[bool]string{
		true:  "was running when checked, but a supervisor still holding the failed stop can stop it afterwards",
		false: "was not running when checked"}[running()]
	return &UpgradeIncomplete{
		Problem: stopFailedText("skep_api", stopErr) + "; skep_api " + now + ": " + detail,
		Check:   "service skep_api status and the API's log (/var/log/daemon.log)",
		Again:   againAPIStopFailed}
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
		if waited == socketNoticeAfter && u.SocketWait > socketNoticeAfter {
			u.printf("  the agent has not answered on its socket after %s; it may be completing interrupted jail stops "+
				"(jail_stop_completing in /var/log/daemon.log); waiting up to %s\n", socketNoticeAfter, u.SocketWait)
		}
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

// manualUpgradeDoc is the procedure for a host skep-setup upgrade does not
// handle: a URL, since an installed host has no source tree to look in.
const manualUpgradeDoc = "https://github.com/greenbeanorg/skep/blob/main/docs/installation/upgrading.md (\"Upgrading a host\", the general steps)"

// defaultDataDirs refuses a service whose rc.conf data directory
// (skep_agent_data_dir, skep_api_data_dir) is not the default: upgrade
// backs up and checks only the default layout.
func defaultDataDirs(root string) error {
	rc, err := ReadRCConf(root)
	if err != nil {
		return err
	}
	for _, v := range []string{"skep_agent_data_dir", "skep_api_data_dir"} {
		d, set := rc.Get(v)
		if !set || path.Clean(d) == config.DefaultDataDir {
			continue
		}
		return fmt.Errorf("%s is %q, not the default %s: skep-setup upgrade handles only the default data directory; upgrade by hand as %s describes",
			v, d, config.DefaultDataDir, manualUpgradeDoc)
	}
	return nil
}

// modulesHint names the guest modules that are configured but not
// loaded, a common reason for a degraded health right after the first
// apply, or "" when there are none or it cannot be told.
func (u *Upgrader) modulesHint(ctx context.Context) string {
	f, err := u.Prober.Probe(ctx)
	if err != nil {
		return ""
	}
	return modulesHint(u.Root, f)
}
