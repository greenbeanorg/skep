// Command retention-test checks retention (ADR 0007) on a real FreeBSD host
// under the rc.d services: the ownership and modes of the two audit archive
// directories, sealing (write, sync, rename) on the file system that holds
// the data directory, pruning of old tasks, removal of old access audit
// entries, the audit commands, the syslog line of each seal, and a restart.
//
// It runs as root. It works in a data directory and a run directory of its
// own, which it creates and which must not exist yet, so it never touches
// an installed Skep. It changes neither rc.conf nor the clock: the settings
// are passed to the rc.d scripts in their environment, and the old history
// it needs is written into the databases before the services start.
//
// See README.md in this directory for how to run it and for recorded
// results.
package main

import (
	"context"
	"database/sql"
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
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/sysaccount"
	"github.com/greenbeanorg/skep/migrations"
	"github.com/greenbeanorg/skep/pkg/models"
)

type options struct {
	dataDir, runDir      string
	agentBin, apiBin     string
	agentRC, apiRC       string
	user, apiAddr, fsTyp string
	keep                 bool
}

type suite struct {
	opts     options
	uid, gid uint32
	failed   int
	checks   int
	// The months the seeded history lies in, as YYYY-MM.
	oldMonth, lastMonth string
}

// marker is created in the data directory, so that cleanup removes only a
// directory this program made.
const marker = ".skep-retention-test"

const stamp = "2006-01-02T15:04:05.000Z"

func main() {
	var o options
	flag.StringVar(&o.dataDir, "data-dir", "", "data directory to create and use; it must not exist (required)")
	flag.StringVar(&o.runDir, "run-dir", "/var/run/skep-retention-test", "run directory to use; it must not exist")
	flag.StringVar(&o.agentBin, "agent-bin", "", "the skep-agent binary (required)")
	flag.StringVar(&o.apiBin, "api-bin", "", "the skep-api binary (required)")
	flag.StringVar(&o.agentRC, "agent-rc", "/usr/local/etc/rc.d/skep_agent", "the skep_agent rc.d script")
	flag.StringVar(&o.apiRC, "api-rc", "/usr/local/etc/rc.d/skep_api", "the skep_api rc.d script")
	flag.StringVar(&o.user, "user", "skep", "the service account skep-api runs as")
	flag.StringVar(&o.apiAddr, "api-addr", "127.0.0.1:18443", "listen address for skep-api during the test")
	flag.StringVar(&o.fsTyp, "fs", "", "the file system type the data directory must be on, for example ufs or zfs (required)")
	flag.BoolVar(&o.keep, "keep", false, "leave the data directory in place afterwards, for inspection")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: retention-test -data-dir DIR -fs ufs|zfs -agent-bin FILE -api-bin FILE [flags]")
		flag.PrintDefaults()
	}
	flag.Parse()

	if runtime.GOOS != "freebsd" {
		fmt.Fprintln(os.Stderr, "retention-test: this test only means something on FreeBSD")
		os.Exit(2)
	}
	if flag.NArg() == 2 && flag.Arg(0) == "probe" {
		// Run by the test itself under another UID: can this user list
		// the directory?
		if _, err := os.ReadDir(flag.Arg(1)); err != nil {
			fmt.Println("refused:", err)
			os.Exit(1)
		}
		fmt.Println("readable")
		return
	}
	if o.dataDir == "" || o.agentBin == "" || o.apiBin == "" || o.fsTyp == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "retention-test: must run as root (it starts the rc.d services)")
		os.Exit(2)
	}
	s := &suite{opts: o}
	acct, err := sysaccount.Lookup(o.user)
	if err != nil {
		fmt.Fprintln(os.Stderr, "retention-test:", err)
		os.Exit(2)
	}
	s.uid, s.gid = acct.UID, acct.GID

	err = s.run()
	if err != nil {
		fmt.Printf("ABORT  %v\n", err)
	}
	s.stopServices()
	s.cleanup()
	switch {
	case err != nil:
		os.Exit(1)
	case s.failed > 0:
		fmt.Printf("\nFAILED: %d of %d checks failed\n", s.failed, s.checks)
		os.Exit(1)
	}
	fmt.Printf("\nPASS, %d checks\n", s.checks)
}

func (s *suite) check(ok bool, format string, args ...any) bool {
	s.checks++
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

// command runs a program with an explicit argument list and environment.
func command(timeout time.Duration, env []string, cred *syscall.Credential, name string, args ...string) (string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append([]string{"PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin", "HOME=/"}, env...)
	if cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	}
	out, err := cmd.CombinedOutput()
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

func (s *suite) socket() string       { return filepath.Join(s.opts.runDir, "agent.sock") }
func (s *suite) agentDir() string     { return filepath.Join(s.opts.dataDir, "agent") }
func (s *suite) apiDir() string       { return filepath.Join(s.opts.dataDir, "api") }
func (s *suite) agentArchive() string { return filepath.Join(s.agentDir(), "audit-archive") }
func (s *suite) apiArchive() string   { return filepath.Join(s.apiDir(), "audit-archive") }

// rcEnv is what an operator would put in rc.conf. It is passed in the
// environment of the rc.d scripts instead, so rc.conf stays untouched.
func (s *suite) rcEnv() []string {
	return []string{
		"skep_agent_enable=YES",
		"skep_agent_bin=" + s.opts.agentBin,
		"skep_agent_data_dir=" + s.opts.dataDir,
		"skep_agent_run_dir=" + s.opts.runDir,
		"skep_agent_args=-socket " + s.socket(),
		"skep_api_enable=YES",
		"skep_api_bin=" + s.opts.apiBin,
		"skep_api_data_dir=" + s.opts.dataDir,
		"skep_api_run_dir=" + s.opts.runDir,
		"skep_api_args=-agent-socket " + s.socket() + " -listen " + s.opts.apiAddr,
	}
}

func (s *suite) rc(script, verb string) (string, int) {
	out, code := command(90*time.Second, s.rcEnv(), nil, script, verb)
	fmt.Printf("       $ %s %s -> exit %d: %s\n", filepath.Base(script), verb, code, strings.ReplaceAll(out, "\n", " | "))
	return out, code
}

func (s *suite) stopServices() {
	step("stop the services")
	for _, script := range []string{s.opts.apiRC, s.opts.agentRC} {
		if _, err := os.Stat(script); err == nil {
			s.rc(script, "stop")
		}
	}
}

// cleanup removes the two directories this program created.
func (s *suite) cleanup() {
	if s.opts.keep {
		fmt.Printf("\nkept %s for inspection\n", s.opts.dataDir)
	} else if _, err := os.Stat(filepath.Join(s.opts.dataDir, marker)); err == nil {
		if err := os.RemoveAll(s.opts.dataDir); err != nil {
			fmt.Printf("could not remove %s: %v\n", s.opts.dataDir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(s.opts.runDir, marker)); err == nil {
		os.RemoveAll(s.opts.runDir)
	}
}

// checkPath checks the owner, group, and permission bits of a path.
func (s *suite) checkPath(path string, uid, gid uint32, mode fs.FileMode) {
	info, err := os.Lstat(path)
	if err != nil {
		s.check(false, "%s: %v", path, err)
		return
	}
	st := info.Sys().(*syscall.Stat_t)
	s.check(st.Uid == uid && st.Gid == gid && info.Mode().Perm() == mode && info.Mode()&fs.ModeSymlink == 0,
		"%s is %d:%d mode %04o (want %d:%d %04o)", path, st.Uid, st.Gid, info.Mode().Perm(), uid, gid, mode)
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

// openRO opens a database read-only.
func openRO(path string) (*sql.DB, error) {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	return sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?"+q.Encode())
}

func scalar(path, query string, args ...any) (int, error) {
	db, err := openRO(path)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var n int
	err = db.QueryRow(query, args...).Scan(&n)
	return n, err
}

func text(path, query string, args ...any) (string, error) {
	db, err := openRO(path)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var v sql.NullString
	err = db.QueryRow(query, args...).Scan(&v)
	return v.String, err
}

func (s *suite) health() (models.Health, error) {
	var h models.Health
	req, err := http.NewRequest(http.MethodGet, "http://agent"+agentproto.Prefix+"/health", nil)
	if err != nil {
		return h, err
	}
	agentproto.Caller{ActorID: "retention-test", ActorName: "retention-test"}.SetHeaders(req.Header)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", s.socket())
		},
	}}
	resp, err := client.Do(req)
	if err != nil {
		return h, err
	}
	defer resp.Body.Close()
	return h, json.NewDecoder(resp.Body).Decode(&h)
}

// seed creates the data directory and the two databases with history that
// is old enough for retention to act on at the first start.
func (s *suite) seed() error {
	ctx := context.Background()
	now := time.Now().UTC()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	// Five months back: sealed, and for the access log older than 90 days
	// after its month ended. Last month: sealed, and too young to remove.
	old, last := first.AddDate(0, -5, 0), first.AddDate(0, -1, 0)
	s.oldMonth, s.lastMonth = old.Format("2006-01"), last.Format("2006-01")

	for _, dir := range []string{s.opts.dataDir, s.opts.runDir} {
		if _, err := os.Lstat(dir); err == nil {
			return fmt.Errorf("%s exists; this test only works in directories it creates", dir)
		}
	}
	if err := os.Mkdir(s.opts.dataDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.opts.dataDir, marker), nil, 0o644); err != nil {
		return err
	}

	// agent.db: root's.
	db, err := database.Open(ctx, filepath.Join(s.agentDir(), "agent.db"))
	if err != nil {
		return err
	}
	if _, err := database.Migrate(ctx, db, migrations.Agent()); err != nil {
		return err
	}
	for i := range 6 {
		month := old
		if i >= 4 {
			month = last
		}
		if _, err := db.Exec(`INSERT INTO audit_log (occurred_at, actor_id, actor_name, peer_uid, action, outcome, details)
			VALUES (?, 'u1', 'andy', ?, 'vm.start', 'success', '{"seeded":true}')`,
			month.AddDate(0, 0, 9).Add(time.Duration(i)*time.Minute).Format(stamp), s.uid); err != nil {
			return err
		}
	}
	for id, age := range map[string]int{"seeded-old-1": 200, "seeded-old-2": 120, "seeded-recent": 10} {
		finished := now.AddDate(0, 0, -age).Format(stamp)
		steps := []struct {
			query string
			args  []any
		}{
			{`INSERT INTO tasks (id, type, actor_id, actor_name, peer_uid, request_hash, created_at) VALUES (?, 'test.seeded', 'u1', 'andy', 0, 'h', ?)`, []any{id, finished}},
			{`INSERT INTO task_events (task_id, message) VALUES (?, 'one'), (?, 'two'), (?, 'three')`, []any{id, id, id}},
			{`UPDATE tasks SET status = 'running' WHERE id = ?`, []any{id}},
			{`UPDATE tasks SET status = 'succeeded', finished_at = ? WHERE id = ?`, []any{finished, id}},
		}
		for _, st := range steps {
			if _, err := db.Exec(st.query, st.args...); err != nil {
				return fmt.Errorf("%s: %w", st.query, err)
			}
		}
	}
	if err := db.Close(); err != nil {
		return err
	}

	// api.db: the service account's, as the rc.d script would create it.
	db, err = database.Open(ctx, filepath.Join(s.apiDir(), "api.db"))
	if err != nil {
		return err
	}
	if _, err := database.Migrate(ctx, db, migrations.API()); err != nil {
		return err
	}
	for i := range 7 {
		month := old
		if i >= 5 {
			month = last
		}
		if _, err := db.Exec(`INSERT INTO audit_log (occurred_at, actor_name, action, outcome, source_ip, details)
			VALUES (?, 'mallory', 'auth.login', 'denied', '192.0.2.7', '{"seeded":true}')`,
			month.AddDate(0, 0, 9).Add(time.Duration(i)*time.Minute).Format(stamp)); err != nil {
			return err
		}
	}
	if err := db.Close(); err != nil {
		return err
	}
	return filepath.WalkDir(s.apiDir(), func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, int(s.uid), int(s.gid))
	})
}

// archiveFiles lists an archive directory and checks every file in it.
func (s *suite) archiveFiles(dir string, uid, gid uint32, want int) []string {
	s.checkPath(dir, uid, gid, 0o700)
	entries, err := os.ReadDir(dir)
	if err != nil {
		s.check(false, "%s: %v", dir, err)
		return nil
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		s.checkPath(filepath.Join(dir, e.Name()), uid, gid, 0o400)
		s.check(strings.HasPrefix(e.Name(), "audit-") && strings.HasSuffix(e.Name(), ".jsonl.gz"),
			"%s is a segment and not a leftover temporary file", e.Name())
	}
	s.check(len(names) == want, "%s holds %d files, want %d: %v", dir, len(names), want, names)
	return names
}

// syslogHas reports whether the notice line of a seal with this hash
// reached /var/log/messages. With the stock syslog.conf that file takes
// *.notice and not daemon.info, so the service's own JSON log line about
// the same seal, which daemon(8) forwards at info, cannot be mistaken for
// it; the line written by the service itself also has its own shape.
func syslogHas(hash string) (string, bool) {
	const file = "/var/log/messages"
	data, err := os.ReadFile(file)
	if err != nil {
		return err.Error(), false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "audit segment sealed: log=") && strings.Contains(line, "sha256="+hash) {
			return file + ": " + line, true
		}
	}
	return "no such line in " + file, false
}

type retentionTask struct {
	ID, Status, ErrorCode, Result, Details string
}

// retentionTasks returns the maintenance.retention tasks, oldest first,
// each with the details of its "finished" audit entry.
func (s *suite) retentionTasks() ([]retentionTask, error) {
	db, err := openRO(filepath.Join(s.agentDir(), "agent.db"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT t.id, t.status, COALESCE(t.error_code, ''), COALESCE(t.result, ''),
			COALESCE((SELECT details FROM audit_log a WHERE a.task_id = t.id AND a.phase = 'finished'), '')
		FROM tasks t WHERE t.type = 'maintenance.retention' ORDER BY t.created_at, t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []retentionTask
	for rows.Next() {
		var t retentionTask
		if err := rows.Scan(&t.ID, &t.Status, &t.ErrorCode, &t.Result, &t.Details); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// waitRetention waits until n retention tasks exist and all are finished.
func (s *suite) waitRetention(n int) ([]retentionTask, bool) {
	var tasks []retentionTask
	ok := waitFor(60*time.Second, func() bool {
		var err error
		tasks, err = s.retentionTasks()
		if err != nil || len(tasks) != n {
			return false
		}
		for _, t := range tasks {
			if t.Status == "pending" || t.Status == "running" {
				return false
			}
		}
		return true
	})
	return tasks, ok
}

func (s *suite) run() error {
	agentDB, apiDB := filepath.Join(s.agentDir(), "agent.db"), filepath.Join(s.apiDir(), "api.db")

	step("seed %s with old history; nothing here changes the clock", s.opts.dataDir)
	if err := s.seed(); err != nil {
		return err
	}
	fmt.Printf("       old month %s, last month %s, today %s\n", s.oldMonth, s.lastMonth, time.Now().UTC().Format("2006-01-02"))
	out, _ := command(10*time.Second, nil, nil, "/bin/df", "-T", s.opts.dataDir)
	fmt.Printf("       $ df -T %s\n       %s\n", s.opts.dataDir, strings.ReplaceAll(out, "\n", "\n       "))
	fields := strings.Fields(out[strings.LastIndex(out, "\n")+1:])
	if !s.check(len(fields) > 1 && fields[1] == s.opts.fsTyp, "the data directory is on %s (want %s)", strings.Join(fields[:min(2, len(fields))], " "), s.opts.fsTyp) {
		return errors.New("wrong file system; nothing was started")
	}

	step("start skep_agent and skep_api through their rc.d scripts")
	if _, code := s.rc(s.opts.agentRC, "start"); code != 0 {
		return errors.New("skep_agent did not start")
	}
	// The run directory is the test's own from here on.
	if err := os.WriteFile(filepath.Join(s.opts.runDir, marker), nil, 0o644); err != nil {
		return err
	}
	if _, code := s.rc(s.opts.apiRC, "start"); code != 0 {
		return errors.New("skep_api did not start")
	}
	if !s.check(waitFor(30*time.Second, func() bool { h, err := s.health(); return err == nil && h.Status != "" }), "the agent answers on %s", s.socket()) {
		return errors.New("the agent is not reachable")
	}

	step("the agent's retention task: tasks pruned, audit segments sealed, nothing removed")
	tasks, ok := s.waitRetention(1)
	if !s.check(ok && len(tasks) == 1, "one maintenance.retention task ran after start (%d found)", len(tasks)) {
		return errors.New("the retention task did not finish")
	}
	first := tasks[0]
	s.check(first.Status == "succeeded", "it succeeded: status=%s error_code=%q", first.Status, first.ErrorCode)
	var result struct {
		Tasks struct {
			RetentionDays int   `json:"retention_days"`
			TasksRemoved  int64 `json:"tasks_removed"`
			EventsRemoved int64 `json:"events_removed"`
		} `json:"tasks"`
		Audit struct {
			Verified int `json:"segments_verified"`
			Sealed   []struct {
				Month   string `json:"month"`
				FirstID int64  `json:"first_id"`
				LastID  int64  `json:"last_id"`
				Entries int64  `json:"entries"`
				Hash    string `json:"hash"`
				File    string `json:"file_name"`
			} `json:"sealed"`
			Removed struct {
				Entries int64 `json:"entries"`
			} `json:"removed"`
		} `json:"audit"`
	}
	if err := json.Unmarshal([]byte(first.Result), &result); err != nil {
		return fmt.Errorf("retention result %q: %w", first.Result, err)
	}
	fmt.Printf("       result: %s\n", first.Result)
	s.check(result.Tasks.RetentionDays == 90 && result.Tasks.TasksRemoved == 2 && result.Tasks.EventsRemoved == 6,
		"tasks: %d removed with %d events at %d days (want 2, 6, 90)", result.Tasks.TasksRemoved, result.Tasks.EventsRemoved, result.Tasks.RetentionDays)
	left, err := scalar(agentDB, `SELECT COUNT(*) FROM tasks WHERE type = 'test.seeded'`)
	recent, _ := scalar(agentDB, `SELECT COUNT(*) FROM tasks WHERE id = 'seeded-recent'`)
	s.check(err == nil && left == 1 && recent == 1, "of the seeded tasks only the recent one is left (%d left, err=%v)", left, err)
	orphans, _ := scalar(agentDB, `SELECT COUNT(*) FROM task_events WHERE task_id NOT IN (SELECT id FROM tasks)`)
	s.check(orphans == 0, "no events without a task (%d)", orphans)

	sealed := result.Audit.Sealed
	if !s.check(len(sealed) == 2 && sealed[0].Month == s.oldMonth && sealed[1].Month == s.lastMonth,
		"two segments sealed, for %s and %s: %+v", s.oldMonth, s.lastMonth, sealed) {
		return errors.New("the agent's segments were not sealed")
	}
	s.check(sealed[0].FirstID == 1 && sealed[0].LastID == 4 && sealed[1].FirstID == 5 && sealed[1].LastID == 6,
		"their ID ranges are 1-4 and 5-6 (%d-%d, %d-%d)", sealed[0].FirstID, sealed[0].LastID, sealed[1].FirstID, sealed[1].LastID)
	s.check(result.Audit.Removed.Entries == 0, "no live audit entry was removed: the agent's default (%d)", result.Audit.Removed.Entries)
	live, _ := scalar(agentDB, `SELECT COUNT(*) FROM audit_log WHERE id <= 6`)
	s.check(live == 6, "all 6 sealed entries are still in the live table (%d)", live)
	s.check(strings.Contains(first.Details, `"tasks_removed":2`) && strings.Contains(first.Details, `"tasks_cutoff"`) &&
		strings.Contains(first.Details, sealed[1].Hash), "the finished audit entry records the cutoff, the counts, and the seals: %s", first.Details)

	step("the agent's archive directory: root only, written with sync and rename on %s", s.opts.fsTyp)
	s.checkPath(s.agentDir(), 0, 0, 0o700)
	names := s.archiveFiles(s.agentArchive(), 0, 0, 2)
	for i, seg := range sealed {
		s.check(i < len(names) && contains(names, seg.File), "segment file %s exists", seg.File)
		line, found := syslogHas(seg.Hash)
		s.check(found, "the seal of %s is in syslog at notice: %s", seg.Month, line)
	}
	probe, code := command(10*time.Second, nil, &syscall.Credential{Uid: s.uid, Gid: s.gid, NoSetGroups: false, Groups: []uint32{s.gid}},
		selfPath(), "probe", s.agentArchive())
	s.check(code != 0, "uid %d (%s) cannot read the agent's archive: %s", s.uid, s.opts.user, probe)

	step("skep-agent audit verify and export, while the agent runs")
	out, code = command(30*time.Second, nil, nil, s.opts.agentBin, "audit", "verify", "-data-dir", s.opts.dataDir)
	fmt.Printf("       %s\n", strings.ReplaceAll(out, "\n", "\n       "))
	s.check(code == 0 && strings.Contains(out, "2 ok, 0 missing, 0 invalid"), "audit verify: exit %d", code)
	out, code = command(30*time.Second, nil, nil, s.opts.agentBin, "audit", "export", "-data-dir", s.opts.dataDir)
	s.check(code == 0 && strings.Count(out, "\n")+1 == 6 && strings.Contains(out, `"details":{"seeded":true}`), "audit export prints the 6 archived entries (exit %d, %d lines)", code, strings.Count(out, "\n")+1)
	h, err := s.health()
	s.check(err == nil && h.Checks[models.CheckAuditArchive].Status == models.HealthOK, "health check %s: %+v (err=%v)", models.CheckAuditArchive, h.Checks[models.CheckAuditArchive], err)

	step("skep-api: its own archive, as %s; entries older than 90 days removed after sealing", s.opts.user)
	if !s.check(waitFor(60*time.Second, func() bool {
		n, err := scalar(apiDB, `SELECT COUNT(*) FROM audit_log WHERE action = 'audit.archive'`)
		return err == nil && n > 0
	}), "skep-api recorded an archive run in its access audit log") {
		return errors.New("skep-api did not archive")
	}
	entry, _ := text(apiDB, `SELECT outcome || ' ' || details FROM audit_log WHERE action = 'audit.archive' ORDER BY id LIMIT 1`)
	fmt.Printf("       audit.archive: %s\n", entry)
	s.check(strings.HasPrefix(entry, "success ") && strings.Contains(entry, `"entries_removed":5`) && strings.Contains(entry, `"live_days":90`),
		"the run succeeded and removed the 5 entries of %s", s.oldMonth)
	segs, _ := scalar(apiDB, `SELECT COUNT(*) FROM audit_segments`)
	oldLive, _ := scalar(apiDB, `SELECT COUNT(*) FROM audit_log WHERE id <= 5`)
	lastLive, _ := scalar(apiDB, `SELECT COUNT(*) FROM audit_log WHERE id IN (6, 7)`)
	s.check(segs == 2 && oldLive == 0 && lastLive == 2, "2 segments; %s gone from the live table (%d left); %s still live (%d of 2)", s.oldMonth, oldLive, s.lastMonth, lastLive)
	s.checkPath(s.apiDir(), s.uid, s.gid, 0o700)
	s.archiveFiles(s.apiArchive(), s.uid, s.gid, 2)
	apiHash, _ := text(apiDB, `SELECT hash FROM audit_segments ORDER BY seq DESC LIMIT 1`)
	line, found := syslogHas(apiHash)
	s.check(found, "skep-api's seal is in syslog, sent as an unprivileged user: %s", line)
	node, _ := text(apiDB, `SELECT node_id FROM audit_segments LIMIT 1`)
	agentNode, _ := text(agentDB, `SELECT id FROM nodes WHERE is_local = 1`)
	s.check(node != "" && node == agentNode, "skep-api's segments carry the node ID it got from the agent (%s)", node)
	out, code = command(30*time.Second, nil, nil, s.opts.apiBin, "audit", "verify", "-data-dir", s.opts.dataDir)
	fmt.Printf("       %s\n", strings.ReplaceAll(out, "\n", "\n       "))
	s.check(code == 0 && strings.Contains(out, "2 ok, 0 missing, 0 invalid"), "skep-api audit verify, started as root, gives up root and verifies: exit %d", code)
	out, code = command(30*time.Second, nil, nil, s.opts.apiBin, "audit", "export", "-data-dir", s.opts.dataDir, "-month", s.oldMonth)
	s.check(code == 0 && strings.Count(out, "\n")+1 == 5, "the removed entries of %s are read back from the archive (%d lines, exit %d)", s.oldMonth, strings.Count(out, "\n")+1, code)
	rootOwned := 0
	filepath.WalkDir(s.apiDir(), func(path string, _ fs.DirEntry, err error) error {
		if info, lerr := os.Lstat(path); err == nil && lerr == nil && info.Sys().(*syscall.Stat_t).Uid != s.uid {
			rootOwned++
		}
		return nil
	})
	s.check(rootOwned == 0, "nothing under %s belongs to anyone but %s after the root-started commands (%d)", s.apiDir(), s.opts.user, rootOwned)

	step("restart the agent: a second run verifies the archive and seals nothing new")
	before := map[string]string{}
	for _, name := range names {
		if data, err := os.ReadFile(filepath.Join(s.agentArchive(), name)); err == nil {
			before[name] = string(data)
		}
	}
	if _, code := s.rc(s.opts.agentRC, "restart"); code != 0 {
		return errors.New("skep_agent did not restart")
	}
	tasks, ok = s.waitRetention(2)
	if !s.check(ok && len(tasks) == 2, "a second retention task ran after the restart (%d found)", len(tasks)) {
		return errors.New("no retention task after the restart")
	}
	second := tasks[1]
	fmt.Printf("       result: %s\n", second.Result)
	s.check(second.Status == "succeeded" && strings.Contains(second.Result, `"segments_verified":2`) && !strings.Contains(second.Result, `"sealed"`) &&
		strings.Contains(second.Result, `"tasks_removed":0`), "it verified the 2 segments, sealed none, and removed nothing: status=%s", second.Status)
	unchanged := len(before) == 2
	for name, data := range before {
		now, err := os.ReadFile(filepath.Join(s.agentArchive(), name))
		unchanged = unchanged && err == nil && string(now) == data
	}
	s.check(unchanged, "the segment files are byte for byte what they were before the restart")
	s.archiveFiles(s.agentArchive(), 0, 0, 2)

	step("alter a segment and restart: the task fails, nothing is removed, health is degraded")
	victim := filepath.Join(s.agentArchive(), names[0])
	if err := os.Chmod(victim, 0o600); err != nil {
		return err
	}
	data := []byte(before[names[0]])
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(victim, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(victim, 0o400); err != nil {
		return err
	}
	if _, code := s.rc(s.opts.agentRC, "restart"); code != 0 {
		return errors.New("skep_agent did not restart")
	}
	tasks, ok = s.waitRetention(3)
	if !s.check(ok && len(tasks) == 3, "a third retention task ran (%d found)", len(tasks)) {
		return errors.New("no retention task after the second restart")
	}
	third := tasks[2]
	s.check(third.Status == "failed" && third.ErrorCode == "audit_archive_invalid", "it failed with audit_archive_invalid: status=%s error_code=%q", third.Status, third.ErrorCode)
	s.check(strings.Contains(third.Details, `"error_code":"audit_archive_invalid"`) && strings.Contains(third.Details, `"audit_entries_removed":0`),
		"its finished audit entry says so: %s", third.Details)
	live, _ = scalar(agentDB, `SELECT COUNT(*) FROM audit_log WHERE id <= 6`)
	s.check(live == 6, "the live entries are untouched (%d of 6)", live)
	s.check(waitFor(15*time.Second, func() bool {
		h, err = s.health()
		return err == nil && h.Status == models.HealthDegraded && h.Checks[models.CheckAuditArchive].Status == models.HealthDegraded
	}), "health is degraded: %+v", h.Checks[models.CheckAuditArchive])
	out, code = command(30*time.Second, nil, nil, s.opts.agentBin, "audit", "verify", "-data-dir", s.opts.dataDir)
	fmt.Printf("       %s\n", strings.ReplaceAll(out, "\n", "\n       "))
	s.check(code != 0 && strings.Contains(out, "invalid  "+s.oldMonth), "audit verify names the altered segment and exits %d", code)
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	return p
}
