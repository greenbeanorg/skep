// Command rcd-test is the native integration test for the skep_agent and
// skep_api rc.d services. It runs on a FreeBSD test host, as root, against
// the installed rc.d scripts and a TEST BUILD of the agent (build tag
// skep_testtasks), and checks what cannot be checked on Linux: the service
// lifecycle, file ownership, the agent socket and its peer checks, the
// privilege drop of skep-api, crash supervision, and behaviour across a
// reboot.
//
// It starts, stops, and kills the two services and removes their run
// directory. It changes nothing else on the host. See README.md.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/pkg/models"
)

type options struct {
	service      string
	runDir       string
	dataDir      string
	user         string
	group        string
	socket       string
	apiService   string
	apiAddr      string
	apiBin       string
	restartDelay time.Duration
	stateFile    string
}

// suite collects check results.
type suite struct {
	opts   options
	failed int
	uid    uint32
	gid    uint32
	// token is the bearer session of the test administrator.
	token string
}

func main() {
	var o options
	flag.StringVar(&o.service, "service", "skep_agent", "rc.d service name")
	flag.StringVar(&o.runDir, "run-dir", "/var/run/skep", "skep_agent_run_dir")
	flag.StringVar(&o.dataDir, "data-dir", "/var/db/skep", "skep_agent_data_dir")
	flag.StringVar(&o.user, "user", "skep", "the service account skep-api runs as (skep_api_runas)")
	flag.StringVar(&o.group, "group", "skep", "skep_agent_rungroup")
	flag.StringVar(&o.socket, "socket", "/var/run/skep/agent.sock", "the agent's socket")
	flag.StringVar(&o.apiService, "api-service", "skep_api", "rc.d service name of skep-api")
	flag.StringVar(&o.apiAddr, "api-addr", "127.0.0.1:8443", "skep-api's listen address")
	flag.StringVar(&o.apiBin, "api-bin", "/usr/local/sbin/skep-api", "the skep-api binary (skep_api_bin)")
	flag.DurationVar(&o.restartDelay, "restart-delay", 5*time.Second, "skep_agent_restart_delay")
	flag.StringVar(&o.stateFile, "state", "", "file that carries state from pre-reboot to post-reboot")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: rcd-test [flags] lifecycle | crashloop | norestart | api | pre-reboot | post-reboot")
		flag.PrintDefaults()
	}
	flag.Parse()

	if runtime.GOOS != "freebsd" {
		fmt.Fprintln(os.Stderr, "rcd-test: this test only means something on FreeBSD")
		os.Exit(2)
	}
	if flag.Arg(0) == "probe" {
		// Run by the test itself under another UID and GID.
		probe(o.socket, flag.Arg(1))
		return
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "rcd-test: must run as root (it drives service(8))")
		os.Exit(2)
	}
	s := &suite{opts: o}
	if err := s.lookupAccount(); err != nil {
		fmt.Fprintln(os.Stderr, "rcd-test:", err)
		os.Exit(2)
	}

	var err error
	switch flag.Arg(0) {
	case "lifecycle":
		err = s.lifecycle()
	case "crashloop":
		err = s.crashLoop()
	case "norestart":
		err = s.noRestart()
	case "api":
		err = s.api()
	case "pre-reboot":
		err = s.preReboot()
	case "post-reboot":
		err = s.postReboot()
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Printf("ABORT  %v\n", err)
		os.Exit(1)
	}
	if s.failed > 0 {
		fmt.Printf("\nFAILED: %d check(s) failed\n", s.failed)
		os.Exit(1)
	}
	fmt.Println("\nPASS")
}

func (s *suite) lookupAccount() error {
	u, err := user.Lookup(s.opts.user)
	if err != nil {
		return fmt.Errorf("service account: %w", err)
	}
	g, err := user.LookupGroup(s.opts.group)
	if err != nil {
		return fmt.Errorf("service group: %w", err)
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(g.Gid, 10, 32)
	if uid == 0 {
		return errors.New("the service account must not be root")
	}
	s.uid, s.gid = uint32(uid), uint32(gid)
	return nil
}

// check records one result.
func (s *suite) check(ok bool, format string, args ...any) bool {
	msg := fmt.Sprintf(format, args...)
	if ok {
		fmt.Printf("ok     %s\n", msg)
	} else {
		s.failed++
		fmt.Printf("FAIL   %s\n", msg)
	}
	return ok
}

func step(format string, args ...any) { fmt.Printf("\n== %s\n", fmt.Sprintf(format, args...)) }

// run executes a command with an explicit argument list.
func run(timeout time.Duration, name string, args ...string) (string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	code := 0
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		code = ee.ExitCode()
	case err != nil:
		code = -1
		out = append(out, []byte(err.Error())...)
	}
	return strings.TrimSpace(string(out)), code
}

func (s *suite) svc(verb string) (string, int) { return service(s.opts.service, verb) }

func (s *suite) apiSvc(verb string) (string, int) { return service(s.opts.apiService, verb) }

func service(name, verb string) (string, int) {
	out, code := run(90*time.Second, "/usr/sbin/service", name, verb)
	fmt.Printf("       $ service %s %s -> exit %d: %s\n", name, verb, code, strings.ReplaceAll(out, "\n", " | "))
	return out, code
}

func (s *suite) supervisorPidfile() string {
	return filepath.Join(s.opts.runDir, "agent-supervisor.pid")
}
func (s *suite) childPidfile() string { return filepath.Join(s.opts.runDir, "agent.pid") }
func (s *suite) dbPath() string       { return filepath.Join(s.opts.dataDir, "agent", "agent.db") }

func readPid(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

func alive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func psField(pid int, field string) string {
	out, _ := run(5*time.Second, "/bin/ps", "-o", field+"=", "-p", strconv.Itoa(pid))
	return strings.TrimSpace(out)
}

// checkPath checks the owner, group, and mode of a file or directory.
func (s *suite) checkPath(path string, uid, gid uint32, mode fs.FileMode) {
	fi, err := os.Stat(path)
	if err != nil {
		s.check(false, "%s exists: %v", path, err)
		return
	}
	st := fi.Sys().(*syscall.Stat_t)
	s.check(st.Uid == uid && st.Gid == gid && fi.Mode().Perm() == mode,
		"%s is %d:%d mode %04o (want %d:%d %04o)", path, st.Uid, st.Gid, fi.Mode().Perm(), uid, gid, mode)
}

func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// agentClient talks to the agent's internal API over its socket. The test
// runs as root, which the agent's peer check allows.
func agentClient(socket string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
	}
}

// agentGet reads one document from the internal API, as the actor
// "rcd-test".
func agentGet(socket, path string, into any) error {
	req, err := http.NewRequest(http.MethodGet, "http://agent"+agentproto.Prefix+path, nil)
	if err != nil {
		return err
	}
	agentproto.Caller{ActorID: "rcd-test", ActorName: "rcd-test"}.SetHeaders(req.Header)
	resp, err := agentClient(socket).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

func (s *suite) getJSON(path string, into any) error { return agentGet(s.opts.socket, path, into) }

func (s *suite) healthy() bool {
	var h models.Health
	return s.getJSON("/health", &h) == nil && h.Status == models.HealthOK
}

// probe is run by the test under another UID and GID. It prints one line:
// "ok" when the agent answered, "refused-connect" when the socket could not
// be opened at all (file permissions), and "refused-peer" when it could but
// the agent dropped the connection (peer credentials).
func probe(socket, readPath string) {
	if readPath != "" {
		// Used to show that an account cannot read another's state.
		f, err := os.Open(readPath)
		if err != nil {
			fmt.Println("refused-open:", err)
			return
		}
		f.Close()
		fmt.Println("ok")
		return
	}
	conn, err := net.DialTimeout("unix", socket, 3*time.Second)
	if err != nil {
		fmt.Println("refused-connect:", err)
		return
	}
	conn.Close()
	var sys models.System
	if err := agentGet(socket, "/system", &sys); err != nil {
		fmt.Println("refused-peer:", err)
		return
	}
	fmt.Println("ok")
}

// probeAs runs probe as the given user and group, with no supplementary
// groups.
func (s *suite) probeAs(uid, gid uint32, readPath string) string {
	self, err := os.Executable()
	if err != nil {
		return "error: " + err.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "-socket", s.opts.socket, "probe", readPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: []uint32{gid}}}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "error: " + err.Error() + ": " + strings.TrimSpace(string(out))
	}
	return strings.TrimSpace(string(out))
}

// checkSocket checks the two layers that protect the agent socket: file
// permissions, and the peer credentials the agent reads from the kernel.
func (s *suite) checkSocket() {
	fi, err := os.Lstat(s.opts.socket)
	if err != nil {
		s.check(false, "%s exists: %v", s.opts.socket, err)
		return
	}
	st := fi.Sys().(*syscall.Stat_t)
	s.check(fi.Mode()&fs.ModeSocket != 0 && st.Uid == 0 && st.Gid == s.gid && fi.Mode().Perm() == 0o660,
		"%s is a socket, %d:%d mode %04o (want 0:%d 0660)", s.opts.socket, st.Uid, st.Gid, fi.Mode().Perm(), s.gid)

	const nobody = 65534
	out := s.probeAs(s.uid, s.gid, "")
	s.check(out == "ok", "uid %d (%s) gid %d: the agent answers (%s)", s.uid, s.opts.user, s.gid, out)
	out = s.probeAs(nobody, nobody, "")
	s.check(strings.HasPrefix(out, "refused-connect"), "uid %d gid %d: file permissions refuse the connection (%s)", nobody, nobody, out)
	// In the socket's group, so the file permissions allow it, but not on
	// the agent's allow-list.
	out = s.probeAs(nobody, s.gid, "")
	s.check(strings.HasPrefix(out, "refused-peer"), "uid %d gid %d: connects, and the agent's peer check drops it (%s)", nobody, s.gid, out)
}

// tcpListeners returns the TCP and UDP sockets a process listens on.
func tcpListeners(pid int) []string {
	out, _ := run(10*time.Second, "/usr/bin/sockstat", "-l", "-P", "tcp,udp")
	var found []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 6 && f[2] == strconv.Itoa(pid) {
			found = append(found, f[4]+" "+f[5])
		}
	}
	return found
}

// taskRow is what the test reads from the agent's database.
type taskRow struct {
	ID, Type, Status, ErrorCode, ActorID string
	PeerUID                              int64
	CreatedAt                            string
}

type auditRow struct {
	Action, Phase, Outcome, TaskStatus, ActorID string
	PeerUID                                     int64
}

// openDB opens the agent's database read-only. The test never writes to it.
func (s *suite) openDB() (*sql.DB, error) {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	return sql.Open("sqlite", "file:"+(&url.URL{Path: s.dbPath()}).EscapedPath()+"?"+q.Encode())
}

func (s *suite) tasks() ([]taskRow, error) {
	db, err := s.openDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, type, status, COALESCE(error_code, ''), actor_id, peer_uid, created_at
		FROM tasks ORDER BY created_at, rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []taskRow
	for rows.Next() {
		var t taskRow
		if err := rows.Scan(&t.ID, &t.Type, &t.Status, &t.ErrorCode, &t.ActorID, &t.PeerUID, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// latest returns the newest task of a type.
func (s *suite) latest(taskType string) (taskRow, bool) {
	all, err := s.tasks()
	if err != nil {
		return taskRow{}, false
	}
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Type == taskType {
			return all[i], true
		}
	}
	return taskRow{}, false
}

func (s *suite) task(id string) (taskRow, bool) {
	all, _ := s.tasks()
	for _, t := range all {
		if t.ID == id {
			return t, true
		}
	}
	return taskRow{}, false
}

func (s *suite) audit(taskID string) ([]auditRow, error) {
	db, err := s.openDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT action, phase, outcome, COALESCE(task_status, ''), actor_id, peer_uid
		FROM audit_log WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var a auditRow
		if err := rows.Scan(&a.Action, &a.Phase, &a.Outcome, &a.TaskStatus, &a.ActorID, &a.PeerUID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *suite) events(taskID string) (messages []string, progress int) {
	db, err := s.openDB()
	if err != nil {
		return nil, 0
	}
	defer db.Close()
	rows, err := db.Query(`SELECT kind, message FROM task_events WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, 0
	}
	defer rows.Close()
	for rows.Next() {
		var kind, msg string
		if rows.Scan(&kind, &msg) != nil {
			return nil, 0
		}
		messages = append(messages, msg)
		if kind == "progress" {
			progress++
		}
	}
	return messages, progress
}

// requireTestBuild refuses to continue unless the configured agent is a
// test build: without the slow task there is nothing to interrupt.
func (s *suite) requireTestBuild() error {
	bin, code := run(5*time.Second, "/usr/sbin/sysrc", "-n", s.opts.service+"_bin")
	if code != 0 {
		bin = "/usr/local/sbin/skep-agent"
	}
	out, code := run(5*time.Second, bin, "-version")
	if code != 0 || !strings.Contains(out, "TEST BUILD") {
		return fmt.Errorf("%s is not a test build of the agent (-version: %q)", bin, out)
	}
	env, _ := run(5*time.Second, "/usr/sbin/sysrc", "-n", s.opts.service+"_env")
	if !strings.Contains(env, "SKEP_TEST_SLEEP=") {
		return fmt.Errorf("%s_env must set SKEP_TEST_SLEEP (it is %q)", s.opts.service, env)
	}
	fmt.Printf("       agent: %s\n       %s\n", bin, strings.ReplaceAll(out, "\n", "\n       "))
	return nil
}

// checkRunning checks everything that must hold while the service is up,
// and returns the supervisor and agent pids.
func (s *suite) checkRunning() (supervisor, child int) {
	s.check(waitFor(20*time.Second, func() bool {
		return alive(readPid(s.supervisorPidfile())) && alive(readPid(s.childPidfile())) && s.healthy()
	}), "the supervisor and the agent are running and the agent answers on its socket")
	supervisor, child = readPid(s.supervisorPidfile()), readPid(s.childPidfile())

	_, code := s.svc("status")
	s.check(code == 0, "service status reports running")

	s.checkPath(s.opts.runDir, 0, s.gid, 0o750)
	s.checkPath(s.opts.dataDir, 0, 0, 0o755)
	// The agent's state belongs to root; the API account cannot open it.
	s.checkPath(filepath.Join(s.opts.dataDir, "agent"), 0, 0, 0o700)
	s.checkPath(s.dbPath(), 0, 0, 0o600)
	out := s.probeAs(s.uid, s.gid, s.dbPath())
	s.check(strings.HasPrefix(out, "refused-open"), "uid %d (%s) cannot open the agent's database (%s)", s.uid, s.opts.user, out)

	s.check(psField(supervisor, "user") == "root" && strings.HasPrefix(psField(supervisor, "command"), "daemon:"),
		"supervisor pid %d is daemon(8) running as root (%s: %s)", supervisor, psField(supervisor, "user"), psField(supervisor, "command"))
	s.check(psField(child, "user") == "root" && psField(child, "ruser") == "root",
		"agent pid %d runs as root (user=%s ruser=%s)", child, psField(child, "user"), psField(child, "ruser"))
	s.check(psField(child, "ppid") == strconv.Itoa(supervisor), "the agent's parent is the supervisor")

	// The agent has no network listener at all.
	listeners := tcpListeners(child)
	s.check(len(listeners) == 0, "the agent listens on no TCP or UDP socket (%v)", listeners)
	s.checkSocket()

	var sys models.System
	err := s.getJSON("/system", &sys)
	s.check(err == nil && sys.Platform == models.PlatformFreeBSD && sys.ProductionHost && !sys.DevMode,
		"system over the socket: platform=%s production_host=%v dev_mode=%v node_id=%s (err=%v)",
		sys.Platform, sys.ProductionHost, sys.DevMode, sys.NodeID, err)
	return supervisor, child
}

func nonLoopbackAddrs() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}

// waitSleepRunning waits until the newest test.sleep task is running and
// has written progress, and it is not the task named by notID.
func (s *suite) waitSleepRunning(notID string) (taskRow, bool) {
	var t taskRow
	ok := waitFor(20*time.Second, func() bool {
		var found bool
		t, found = s.latest("test.sleep")
		if !found || t.ID == notID || t.Status != "running" {
			return false
		}
		_, progress := s.events(t.ID)
		return progress >= 2
	})
	s.check(ok, "a test.sleep task is in flight with progress events: id=%s status=%s", t.ID, t.Status)
	return t, ok
}

// checkInterrupted checks the full record of an interrupted task.
func (s *suite) checkInterrupted(id, wantCode string) {
	t, _ := s.task(id)
	s.check(t.Status == "interrupted" && t.ErrorCode == wantCode,
		"task %s is %s with error code %q (want interrupted, %q)", id, t.Status, t.ErrorCode, wantCode)
	msgs, _ := s.events(id)
	s.check(len(msgs) > 0 && msgs[len(msgs)-1] == "task is interrupted",
		"its last event is %q", last(msgs))
	a, err := s.audit(id)
	ok := err == nil && len(a) == 2 &&
		a[0].Phase == "requested" && a[0].Outcome == "success" &&
		a[1].Phase == "finished" && a[1].Outcome == "failure" && a[1].TaskStatus == "interrupted"
	for _, e := range a {
		ok = ok && e.Action == "test.sleep" && e.ActorID == "system" && e.PeerUID == 0
	}
	s.check(ok, "its audit trail is requested/success then finished/failure(interrupted), actor system, peer uid 0: %+v (err=%v)", a, err)
}

func last(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

func (s *suite) checkRefresh(after string) {
	var t taskRow
	ok := waitFor(20*time.Second, func() bool {
		var found bool
		t, found = s.latest("discovery.refresh")
		return found && t.Status == "succeeded" && t.CreatedAt > after
	})
	s.check(ok && t.ActorID == "system" && t.PeerUID == 0,
		"this start ran discovery.refresh: id=%s status=%s actor=%s peer_uid=%d", t.ID, t.Status, t.ActorID, t.PeerUID)
}

func (s *suite) checkStopped(supervisor, child int) {
	s.check(!alive(supervisor) && !alive(child), "the supervisor (%d) and the agent (%d) are gone", supervisor, child)
	_, e1 := os.Stat(s.supervisorPidfile())
	_, e2 := os.Stat(s.childPidfile())
	s.check(errors.Is(e1, fs.ErrNotExist) && errors.Is(e2, fs.ErrNotExist), "both pid files were removed")
	_, code := s.svc("status")
	s.check(code != 0, "service status reports not running")
	s.check(!s.healthy(), "the agent no longer answers on its socket")
	_, err := os.Lstat(s.opts.socket)
	s.check(errors.Is(err, fs.ErrNotExist), "the socket file was removed (%v)", err)
}

func now() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// lifecycle is the test that needs no reboot.
func (s *suite) lifecycle() error {
	if err := s.requireTestBuild(); err != nil {
		return err
	}

	step("clean start, with the run directory missing as it is after a boot")
	s.svc("stop")
	if err := os.RemoveAll(s.opts.runDir); err != nil {
		return err
	}
	t0 := now()
	_, code := s.svc("start")
	s.check(code == 0, "service start succeeds")
	supervisor, child := s.checkRunning()
	s.checkRefresh(t0)
	_, code = s.svc("start")
	s.check(readPid(s.childPidfile()) == child, "a second start does not start a second agent (exit %d)", code)

	step("kill -9 the agent while a task is in flight")
	first, ok := s.waitSleepRunning("")
	if !ok {
		return errors.New("no in-flight task to interrupt")
	}
	t1 := now()
	if err := syscall.Kill(child, syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill -9 %d: %w", child, err)
	}
	fmt.Printf("       sent SIGKILL to agent pid %d\n", child)
	restarted := waitFor(s.opts.restartDelay+20*time.Second, func() bool {
		pid := readPid(s.childPidfile())
		return pid != 0 && pid != child && alive(pid) && s.healthy()
	})
	s.check(restarted, "the supervisor started a new agent (pid %d, was %d)", readPid(s.childPidfile()), child)
	s.check(readPid(s.supervisorPidfile()) == supervisor && alive(supervisor), "the supervisor itself kept running")
	s.checkInterrupted(first.ID, "agent_restart")
	s.checkRefresh(t1)
	second, ok := s.waitSleepRunning(first.ID)
	if !ok {
		return errors.New("the restarted agent has no in-flight task")
	}
	t, _ := s.task(first.ID)
	s.check(t.Status == "interrupted", "the interrupted task was not resumed")
	_, child = s.checkRunning()

	step("service stop while a task is in flight")
	began := time.Now()
	_, code = s.svc("stop")
	s.check(code == 0, "service stop succeeds (took %s)", time.Since(began).Round(time.Millisecond))
	s.checkStopped(supervisor, child)
	s.checkInterrupted(second.ID, "agent_shutdown")

	step("service restart")
	_, code = s.svc("start")
	s.check(code == 0, "service start succeeds")
	_, childA := s.checkRunning()
	third, _ := s.waitSleepRunning(second.ID)
	t2 := now()
	_, code = s.svc("restart")
	s.check(code == 0, "service restart succeeds")
	supervisor, childB := s.checkRunning()
	s.check(childA != childB, "restart replaced the agent (pid %d -> %d)", childA, childB)
	s.checkInterrupted(third.ID, "agent_shutdown")
	s.checkRefresh(t2)

	step("final stop")
	_, code = s.svc("stop")
	s.check(code == 0, "service stop succeeds")
	s.checkStopped(supervisor, childB)

	all, err := s.tasks()
	if err != nil {
		return err
	}
	unfinished := 0
	for _, t := range all {
		if t.Status == "pending" || t.Status == "running" {
			unfinished++
		}
	}
	// The last sleep task was stopped by the final service stop.
	s.check(unfinished == 0, "no task is left pending or running after the final stop (%d tasks in history)", len(all))
	return nil
}

func (s *suite) sysrc(name string) string {
	out, code := run(5*time.Second, "/usr/sbin/sysrc", "-n", s.opts.service+"_"+name)
	if code != 0 {
		return ""
	}
	return out
}

// crashLoop checks the service when the agent fails at every start. The
// operator sets skep_agent_args to an argument the agent rejects first.
func (s *suite) crashLoop() error {
	const badArg = "-no-such-flag"
	if !strings.Contains(s.sysrc("args"), badArg) {
		return fmt.Errorf("set %s_args=%q first, so that the agent fails at every start", s.opts.service, badArg)
	}
	if s.opts.restartDelay <= 0 {
		return errors.New("this phase needs supervision (-restart-delay > 0)")
	}

	step("start with an agent that exits at once")
	s.svc("stop")
	out, code := s.svc("start")
	s.check(code != 0 && strings.Contains(out, "not running after start"),
		"service start reports that the agent is not running (exit %d)", code)
	supervisor := readPid(s.supervisorPidfile())
	s.check(alive(supervisor) && psField(supervisor, "user") == "root", "the supervisor is running (pid %d)", supervisor)
	out, code = s.svc("status")
	s.check(code != 0 && strings.Contains(out, "has exited"), "service status does not claim that the agent is running")
	s.check(!s.healthy(), "the agent does not answer")

	step("the supervisor keeps retrying and can be stopped")
	time.Sleep(2*s.opts.restartDelay + time.Second)
	s.check(alive(supervisor) && readPid(s.supervisorPidfile()) == supervisor,
		"the supervisor is still running after two restart delays")
	s.check(!alive(readPid(s.childPidfile())), "no agent is left running between attempts")
	_, code = s.svc("stop")
	s.check(code == 0, "service stop succeeds")
	s.checkStopped(supervisor, 0)
	return nil
}

// noRestart checks skep_agent_restart_delay=0: a killed agent stays down.
func (s *suite) noRestart() error {
	if s.sysrc("restart_delay") != "0" {
		return fmt.Errorf("set %s_restart_delay=0 first", s.opts.service)
	}
	if err := s.requireTestBuild(); err != nil {
		return err
	}

	step("start without crash supervision")
	s.svc("stop")
	_, code := s.svc("start")
	s.check(code == 0, "service start succeeds")
	supervisor, child := s.checkRunning()
	sleep, ok := s.waitSleepRunning("")
	if !ok {
		return errors.New("no in-flight task")
	}

	step("kill -9 the agent: it must stay down")
	if err := syscall.Kill(child, syscall.SIGKILL); err != nil {
		return err
	}
	fmt.Printf("       sent SIGKILL to agent pid %d\n", child)
	s.check(waitFor(10*time.Second, func() bool { return !alive(supervisor) && !alive(child) }),
		"the supervisor exited with the agent")
	time.Sleep(2 * time.Second)
	s.checkStopped(supervisor, child)
	t, _ := s.task(sleep.ID)
	s.check(t.Status == "running", "the killed agent's task is still recorded as running until the next start (%s)", t.Status)

	step("the next start sweeps the task")
	_, code = s.svc("start")
	s.check(code == 0, "service start succeeds")
	supervisor, child = s.checkRunning()
	s.checkInterrupted(sleep.ID, "agent_restart")
	_, code = s.svc("stop")
	s.check(code == 0, "service stop succeeds")
	s.checkStopped(supervisor, child)
	return nil
}

// rebootState is carried across the reboot in the state file.
type rebootState struct {
	BootTime  string `json:"boot_time"`
	NodeID    string `json:"node_id"`
	SleepTask string `json:"sleep_task"`
	Tasks     int    `json:"tasks"`
	SavedAt   string `json:"saved_at"`
}

func bootTime() string {
	out, _ := run(5*time.Second, "/sbin/sysctl", "-n", "kern.boottime")
	return out
}

// preReboot leaves the service running with a task in flight and records
// what postReboot compares against. The operator then reboots the host.
func (s *suite) preReboot() error {
	if s.opts.stateFile == "" {
		return errors.New("-state is required")
	}
	if err := s.requireTestBuild(); err != nil {
		return err
	}
	step("service enabled and running before the reboot")
	out, _ := run(5*time.Second, "/usr/sbin/sysrc", "-n", s.opts.service+"_enable")
	s.check(strings.EqualFold(out, "YES"), "%s_enable is YES (%q)", s.opts.service, out)
	if _, code := s.svc("status"); code != 0 {
		_, code = s.svc("start")
		s.check(code == 0, "service start succeeds")
	}
	s.checkRunning()
	sleep, ok := s.waitSleepRunning("")
	if !ok {
		return errors.New("no in-flight task")
	}
	var sys models.System
	if err := s.getJSON("/system", &sys); err != nil {
		return err
	}
	all, err := s.tasks()
	if err != nil {
		return err
	}
	st := rebootState{BootTime: bootTime(), NodeID: sys.NodeID, SleepTask: sleep.ID, Tasks: len(all), SavedAt: now()}
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := os.WriteFile(s.opts.stateFile, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("       state saved to %s: %s\n", s.opts.stateFile, strings.ReplaceAll(string(b), "\n", " "))
	fmt.Println("\nNow reboot the host, then run: rcd-test -state FILE post-reboot")
	return nil
}

// postReboot checks that the service came back by itself after a reboot.
// It does not start anything.
func (s *suite) postReboot() error {
	if s.opts.stateFile == "" {
		return errors.New("-state is required")
	}
	b, err := os.ReadFile(s.opts.stateFile)
	if err != nil {
		return err
	}
	var st rebootState
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}

	step("the host rebooted and the service started at boot")
	s.check(bootTime() != st.BootTime && st.BootTime != "", "kern.boottime changed: %s", bootTime())
	s.checkRunning()

	step("state from before the reboot")
	var sys models.System
	err = s.getJSON("/system", &sys)
	s.check(err == nil && sys.NodeID == st.NodeID, "the node ID is unchanged: %s", sys.NodeID)
	all, err := s.tasks()
	if err != nil {
		return err
	}
	s.check(len(all) > st.Tasks, "task history survived and grew: %d tasks before, %d now", st.Tasks, len(all))
	// rc.shutdown stops the service (KEYWORD: shutdown), so the agent had
	// time to record the task itself.
	s.checkInterrupted(st.SleepTask, "agent_shutdown")
	s.checkRefresh(st.SavedAt)
	s.waitSleepRunning(st.SleepTask)
	return nil
}

func (s *suite) apiSupervisorPidfile() string {
	return filepath.Join(s.opts.runDir, "api-supervisor.pid")
}
func (s *suite) apiChildPidfile() string { return filepath.Join(s.opts.runDir, "api.pid") }

// apiClient trusts exactly the certificate skep-api generated, which the
// test can read because it runs as root.
func (s *suite) apiClient() (*http.Client, error) {
	pemBytes, err := os.ReadFile(filepath.Join(s.opts.dataDir, "api", "tls", "cert.pem"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, errors.New("the generated certificate is not PEM")
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13},
	}}, nil
}

// apiGet calls the public API, with the session token when the test has
// logged in, and returns the status and the decoded body.
func (s *suite) apiGet(path string, into any) (int, error) {
	c, err := s.apiClient()
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequest(http.MethodGet, "https://"+s.opts.apiAddr+path, nil)
	if err != nil {
		return 0, err
	}
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(into)
}

// testUser is the administrator the test creates with the local command.
const testUser = "rcd-test"

// createAdminAndLogin creates (or resets) the test administrator with
// "skep-api admin ...", run as root, and logs in over the API.
func (s *suite) createAdminAndLogin() error {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	password := base64.RawURLEncoding.EncodeToString(raw)
	admin := func(verb string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, s.opts.apiBin, "admin", verb, "--username", testUser, "--password-stdin", "-data-dir", s.opts.dataDir)
		cmd.Stdin = strings.NewReader(password + "\n")
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	out, err := admin("create")
	if err != nil && strings.Contains(out, "already exists") {
		out, err = admin("reset-password")
	}
	s.check(err == nil, "'skep-api admin' run as root: %s", out)
	if err != nil {
		return err
	}
	// The command gave up root before it opened anything: what it wrote
	// belongs to the service account.
	for _, f := range []string{"api.db", "api.db-wal"} {
		if _, statErr := os.Stat(filepath.Join(s.opts.dataDir, "api", f)); statErr == nil {
			s.checkPath(filepath.Join(s.opts.dataDir, "api", f), s.uid, s.gid, 0o600)
		}
	}

	c, err := s.apiClient()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(models.LoginRequest{Username: testUser, Password: password, Session: models.SessionBearer})
	resp, err := c.Post("https://"+s.opts.apiAddr+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var session models.SessionResponse
	err = json.NewDecoder(resp.Body).Decode(&session)
	s.check(err == nil && resp.StatusCode == http.StatusOK && session.Token != "" && session.User.Role == "admin",
		"login over the API as %s: status %d, role %q", testUser, resp.StatusCode, session.User.Role)
	s.token = session.Token
	return nil
}

func (s *suite) apiAnswers() bool {
	var h models.HealthStatus
	code, err := s.apiGet("/api/v1/health", &h)
	return err == nil && code != 0
}

// checkAPIRunning checks everything that must hold while skep_api is up,
// and returns the supervisor and server pids.
func (s *suite) checkAPIRunning() (supervisor, child int) {
	s.check(waitFor(20*time.Second, func() bool {
		return alive(readPid(s.apiSupervisorPidfile())) && alive(readPid(s.apiChildPidfile())) && s.apiAnswers()
	}), "the supervisor and skep-api are running and the API answers on %s", s.opts.apiAddr)
	supervisor, child = readPid(s.apiSupervisorPidfile()), readPid(s.apiChildPidfile())

	_, code := s.apiSvc("status")
	s.check(code == 0, "service status reports running")
	s.checkPath(s.opts.runDir, 0, s.gid, 0o750)
	s.checkPath(filepath.Join(s.opts.dataDir, "api"), s.uid, s.gid, 0o700)
	s.checkPath(filepath.Join(s.opts.dataDir, "api", "api.db"), s.uid, s.gid, 0o600)
	// The TLS key was generated by skep-api itself, as the service
	// account, and nobody else can read it.
	s.checkPath(filepath.Join(s.opts.dataDir, "api", "tls"), s.uid, s.gid, 0o700)
	s.checkPath(filepath.Join(s.opts.dataDir, "api", "tls", "key.pem"), s.uid, s.gid, 0o600)

	s.check(psField(supervisor, "user") == "root" && strings.HasPrefix(psField(supervisor, "command"), "daemon:"),
		"supervisor pid %d is daemon(8) running as root (%s: %s)", supervisor, psField(supervisor, "user"), psField(supervisor, "command"))
	s.check(psField(child, "user") == s.opts.user && psField(child, "ruser") == s.opts.user,
		"skep-api pid %d runs as %s, not root (user=%s ruser=%s)", child, s.opts.user, psField(child, "user"), psField(child, "ruser"))
	s.check(psField(child, "ppid") == strconv.Itoa(supervisor), "skep-api's parent is the supervisor")

	listeners := tcpListeners(child)
	s.check(len(listeners) == 1 && strings.HasSuffix(listeners[0], " "+s.opts.apiAddr),
		"skep-api listens on %s and nothing else (%v)", s.opts.apiAddr, listeners)
	_, port, _ := net.SplitHostPort(s.opts.apiAddr)
	for _, a := range nonLoopbackAddrs() {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(a, port), 2*time.Second)
		if err == nil {
			conn.Close()
		}
		s.check(err != nil, "the API is not reachable on %s:%s", a, port)
	}
	return supervisor, child
}

func (s *suite) checkAPIStopped(supervisor, child int) {
	s.check(!alive(supervisor) && !alive(child), "the supervisor (%d) and skep-api (%d) are gone", supervisor, child)
	_, e1 := os.Stat(s.apiSupervisorPidfile())
	_, e2 := os.Stat(s.apiChildPidfile())
	s.check(errors.Is(e1, fs.ErrNotExist) && errors.Is(e2, fs.ErrNotExist), "both pid files were removed")
	_, code := s.apiSvc("status")
	s.check(code != 0, "service status reports not running")
	s.check(!s.apiAnswers(), "the API no longer answers")
}

// api checks the skep_api service together with skep_agent: skep-api runs
// unprivileged, serves the public API through the agent socket, and each
// service can start first or restart alone.
func (s *suite) api() error {
	step("skep-api refuses to run as root")
	out, code := run(10*time.Second, s.opts.apiBin, "-data-dir", s.opts.dataDir)
	s.check(code != 0 && strings.Contains(out, "refusing to run as root"), "started directly as root: exit %d, %q", code, out)

	step("skep_api starts first, with the agent down")
	s.apiSvc("stop")
	s.svc("stop")
	if err := os.RemoveAll(s.opts.runDir); err != nil {
		return err
	}
	_, code = s.apiSvc("start")
	s.check(code == 0, "service start succeeds")
	_, child := s.checkAPIRunning()
	var anonymous map[string]any
	status, err := s.apiGet("/api/v1/health", &anonymous)
	s.check(err == nil && status == http.StatusServiceUnavailable && len(anonymous) == 1 && anonymous["status"] == models.HealthDegraded,
		"health without a session and without the agent: status %d, body %v (the status only)", status, anonymous)
	var apiErr models.ErrorResponse
	status, err = s.apiGet("/api/v1/system", &apiErr)
	s.check(err == nil && status == http.StatusUnauthorized && apiErr.Error.Code == "unauthenticated",
		"system without a session: status %d, code %q (err=%v)", status, apiErr.Error.Code, err)

	step("an administrator is created on the host and logs in, with the agent still down")
	if err := s.createAdminAndLogin(); err != nil {
		return err
	}
	var health models.Health
	status, err = s.apiGet("/api/v1/health", &health)
	s.check(err == nil && status == http.StatusServiceUnavailable && health.Checks["agent"].Status == models.HealthDegraded,
		"health with a session: status %d, agent check %q (err=%v)", status, health.Checks["agent"].Status, err)
	status, err = s.apiGet("/api/v1/system", &apiErr)
	s.check(err == nil && status == http.StatusServiceUnavailable && apiErr.Error.Code == models.CodeAgentUnavailable,
		"system without the agent: status %d, code %q (err=%v)", status, apiErr.Error.Code, err)

	step("the agent starts; the API serves the node through the socket")
	_, code = s.svc("start")
	s.check(code == 0, "service start succeeds")
	s.checkRunning()
	var sys, direct models.System
	ok := waitFor(10*time.Second, func() bool {
		status, err = s.apiGet("/api/v1/system", &sys)
		return err == nil && status == http.StatusOK
	})
	_ = s.getJSON("/system", &direct)
	s.check(ok && sys.NodeID != "" && sys.NodeID == direct.NodeID && sys.Platform == models.PlatformFreeBSD && sys.ProductionHost,
		"/api/v1/system: status %d node_id=%s (agent says %s) platform=%s production_host=%v",
		status, sys.NodeID, direct.NodeID, sys.Platform, sys.ProductionHost)
	var hw models.Hardware
	status, err = s.apiGet("/api/v1/nodes/"+sys.NodeID+"/hardware", &hw)
	s.check(err == nil && status == http.StatusOK && hw.NodeID == sys.NodeID, "/api/v1/nodes/{id}/hardware: status %d (err=%v)", status, err)
	var list models.TaskList
	status, err = s.apiGet("/api/v1/tasks", &list)
	s.check(err == nil && status == http.StatusOK && len(list.Items) > 0, "/api/v1/tasks: status %d, %d tasks (err=%v)", status, len(list.Items), err)
	status, _ = s.apiGet("/api/v1/health", &health)
	s.check(status == http.StatusOK && health.Status == models.HealthOK, "health with the agent: status %d, %s", status, health.Status)
	s.check(readPid(s.apiChildPidfile()) == child, "skep-api was not restarted for any of this")

	step("a user cancels a running task through the API; the agent records who")
	if sleep, ok := s.waitSleepRunning(""); ok {
		c, err := s.apiClient()
		if err != nil {
			return err
		}
		req, _ := http.NewRequest(http.MethodPost, "https://"+s.opts.apiAddr+"/api/v1/tasks/"+sleep.ID+"/cancel", nil)
		req.Header.Set("Authorization", "Bearer "+s.token)
		req.Header.Set("X-Request-ID", "rcd-test-cancel")
		resp, err := c.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		s.check(err == nil && resp.StatusCode == http.StatusOK, "POST /api/v1/tasks/{id}/cancel: status %v (err=%v)", statusOf(resp), err)
		s.check(waitFor(15*time.Second, func() bool { t, _ := s.task(sleep.ID); return t.Status == "cancelled" }),
			"task %s is cancelled", sleep.ID)
		var actor, requestID string
		var peer int64
		if db, err := s.openDB(); err == nil {
			_ = db.QueryRow(`SELECT actor_name, peer_uid, COALESCE(request_id, '') FROM audit_log
				WHERE action = 'task.cancel' AND task_id = ?`, sleep.ID).Scan(&actor, &peer, &requestID)
			db.Close()
		}
		// The actor is the user; the peer is skep-api's account, which
		// the agent read from the socket itself.
		s.check(actor == testUser && peer == int64(s.uid) && requestID == "rcd-test-cancel",
			"the agent's audit log: actor %q, peer uid %d (want %d, the %s account), request ID %q", actor, peer, s.uid, s.opts.user, requestID)
	}

	step("the agent restarts alone; the API recovers by itself")
	_, code = s.svc("restart")
	s.check(code == 0, "service restart of the agent succeeds")
	ok = waitFor(15*time.Second, func() bool {
		status, err = s.apiGet("/api/v1/system", &sys)
		return err == nil && status == http.StatusOK
	})
	s.check(ok && readPid(s.apiChildPidfile()) == child, "the same skep-api (pid %d) answers again: status %d", child, status)

	step("kill -9 skep-api: the supervisor starts a new one")
	if err := syscall.Kill(child, syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill -9 %d: %w", child, err)
	}
	fmt.Printf("       sent SIGKILL to skep-api pid %d\n", child)
	restarted := waitFor(s.opts.restartDelay+20*time.Second, func() bool {
		pid := readPid(s.apiChildPidfile())
		return pid != 0 && pid != child && alive(pid) && s.apiAnswers()
	})
	s.check(restarted, "the supervisor started a new skep-api (pid %d, was %d)", readPid(s.apiChildPidfile()), child)
	s.check(alive(readPid(s.childPidfile())), "the agent kept running")
	supervisor, child := s.checkAPIRunning()

	step("stop skep_api; the agent keeps running")
	_, code = s.apiSvc("stop")
	s.check(code == 0, "service stop succeeds")
	s.checkAPIStopped(supervisor, child)
	s.check(s.healthy(), "the agent still answers on its socket")
	_, code = s.svc("stop")
	s.check(code == 0, "service stop of the agent succeeds")
	return nil
}

func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}
