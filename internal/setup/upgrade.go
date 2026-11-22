package setup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	agentDB     = "/var/db/skep/agent/agent.db"
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
	// Force upgrades while tasks are running (they become interrupted).
	Force bool
	Keep  int
	// Sleep and PIDWait pace the wait for a restarted service; Inode
	// reads a file's inode. Tests replace them.
	Sleep   func(time.Duration)
	PIDWait time.Duration
	Inode   func(path string) (uint64, error)
}

// UpgradeRefusal is a reason upgrade did nothing.
type UpgradeRefusal struct{ Reason string }

func (r *UpgradeRefusal) Error() string { return "refused: " + r.Reason + "; nothing was changed" }

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

// runningTasks counts the agent's running tasks, read-only. A missing
// database (never started) has none.
func runningTasks(ctx context.Context, path string) (int, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?"+q.Encode())
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var n int
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE status = 'running'`).Scan(&n)
	return n, err
}

var labelSafeRE = regexp.MustCompile(`[^A-Za-z0-9._+-]`)

// Upgrade runs the steps; any refusal comes before anything changes.
func (u *Upgrader) Upgrade(ctx context.Context) error {
	if u.Keep == 0 {
		u.Keep = dbbackup.DefaultKeep
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
	// 1. Platform and versions.
	f, err := u.Prober.Probe(ctx)
	if err != nil {
		return err
	}
	if it := platformItem(f); it.Kind == Refuse {
		return &UpgradeRefusal{it.What + ": " + it.Detail}
	}
	want := u.Self.Version + "+" + u.Self.Commit
	for _, bin := range []string{agentBin, apiBin} {
		got, err := u.installedBuild(ctx, bin)
		if err != nil {
			return &UpgradeRefusal{err.Error()}
		}
		if got != want {
			return &UpgradeRefusal{fmt.Sprintf("%s is %s, skep-setup is %s: the package was not upgraded as one", bin, got, want)}
		}
	}
	u.printf("  installed: skep-agent, skep-api and skep-setup are %s\n", want)
	// 2. Running tasks.
	n, err := runningTasks(ctx, hostPath(u.Root, agentDB))
	if err != nil {
		return fmt.Errorf("read the agent's tasks: %w", err)
	}
	if n > 0 && !u.Force {
		return &UpgradeRefusal{fmt.Sprintf("%d task(s) are running; wait for them, or use --force (they become interrupted)", n)}
	}
	// 3. Backups, labelled with the running (old) build.
	old, err := u.Agent.Build(ctx)
	if err != nil {
		u.printf("  the running agent's build is unknown (%v)\n", err)
		old = "unknown"
	}
	label := labelSafeRE.ReplaceAllString(old, "_")
	dir := hostPath(u.Root, backupDir) + "/" + label + "-" + u.Now().UTC().Format("20060102T150405Z")
	for _, bin := range []string{agentBin, apiBin} {
		out, err := u.Runner.Run(ctx, Cmd{Name: hostPath(u.Root, bin), Args: []string{"db", "backup", "-dir", dir, "-keep", strconv.Itoa(u.Keep)}})
		if err != nil {
			return fmt.Errorf("backup failed, nothing restarted: %w", err)
		}
		u.printf("%s", indent(out))
	}
	// 4. Ordered restart, each service verified.
	for _, c := range [][]string{{"skep_api", "stop"}, {"skep_agent", "stop"}} {
		if _, err := u.Runner.Run(ctx, Cmd{Name: "service", Args: c}); err != nil {
			return err
		}
		u.printf("  service %s %s\n", c[0], c[1])
	}
	for _, s := range []struct{ svc, bin, pid string }{{"skep_agent", agentBin, agentPID}, {"skep_api", apiBin, apiPID}} {
		if _, err := u.Runner.Run(ctx, Cmd{Name: "service", Args: []string{s.svc, "start"}}); err != nil {
			return err
		}
		if err := u.verify(ctx, s.svc, s.bin, s.pid, want); err != nil {
			return err
		}
	}
	// 5. Health.
	status, err := u.Agent.Health(ctx)
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
	out, err := u.Runner.Run(ctx, Cmd{Name: "fstat", Args: []string{"-p", pid}})
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
