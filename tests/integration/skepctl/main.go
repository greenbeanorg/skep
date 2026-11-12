// Command skepctl-test is the native integration test for skepctl
// (Milestone 4b, ADR 0006 decisions 3, 5, and 6) and for the local
// commands "skep-api admin disable" and "admin delete".
//
// It runs the real skep-agent and skep-api in development mode, as an
// ordinary user, in a scratch directory, with the API on 127.0.0.1:18443,
// and drives the real skepctl binary: on a pseudo-terminal for the trust
// question and the password, and without a controlling terminal for
// everything else. It changes nothing outside its scratch directory and
// removes that when it is done.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"

	"golang.org/x/sys/unix"
)

const (
	listen   = "127.0.0.1:18443"
	server   = "https://" + listen
	password = "Tr0ub4dor&3-horse-staple"
)

var failed int

func check(ok bool, format string, args ...any) bool {
	msg := fmt.Sprintf(format, args...)
	if ok {
		fmt.Printf("ok     %s\n", msg)
	} else {
		failed++
		fmt.Printf("FAIL   %s\n", msg)
	}
	return ok
}

func step(format string, args ...any) { fmt.Printf("\n== %s\n", fmt.Sprintf(format, args...)) }

// cleanups run before an abort, because os.Exit skips deferred calls:
// the processes started and the scratch directory must not outlive the
// test.
var cleanups []func()

func fatal(format string, args ...any) {
	fmt.Printf("ABORT  %s\n", fmt.Sprintf(format, args...))
	exit(1)
}

func exit(code int) {
	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanups[i]()
	}
	os.Exit(code)
}

func short(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 160 {
		return s[:160] + "..."
	}
	return s
}

// env is the test's configuration.
type env struct {
	agent, api, ctl string
	dataDir         string
	ctlDir          string
}

// result is the outcome of one command.
type result struct {
	out  string
	exit int
}

// runNoTTY runs a command in a new session, so it has no controlling
// terminal: /dev/tty cannot be opened, as in a cron job or a pipeline.
func (e *env) runNoTTY(stdin, bin string, args ...string) result {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "SKEPCTL_CONFIG_DIR="+e.ctlDir)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.CombinedOutput()
	exit := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		fatal("run %s: %v", bin, err)
	}
	return result{string(out), exit}
}

func (e *env) ctlRun(args ...string) result { return e.runNoTTY("", e.ctl, args...) }

func (e *env) apiLocal(stdin string, args ...string) result {
	return e.runNoTTY(stdin, e.api, append(args, "-dev", "-data-dir", e.dataDir)...)
}

// screen collects what the terminal shows.
type screen struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *screen) pump(master *os.File) {
	buf := make([]byte, 1024)
	for {
		n, err := master.Read(buf)
		s.mu.Lock()
		s.buf.Write(buf[:n])
		s.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (s *screen) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

type term struct {
	master, slave *os.File
	cmd           *exec.Cmd
	shown         screen
	done          chan error
}

// wrapper keeps the session leader alive after the command, so that the
// terminal is not revoked before its output has been read.
const wrapper = `trap : INT; "$0" "$@"; echo "skep-exit=$?"; sleep 3`

func (e *env) onTerminal(args ...string) *term {
	master, slave, err := openPTY()
	if err != nil {
		fatal("open a pseudo-terminal: %v", err)
	}
	cmd := exec.Command("/bin/sh", append([]string{"-c", wrapper, e.ctl}, args...)...)
	cmd.Env = append(os.Environ(), "SKEPCTL_CONFIG_DIR="+e.ctlDir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		fatal("start skepctl: %v", err)
	}
	t := &term{master: master, slave: slave, cmd: cmd, done: make(chan error, 1)}
	go t.shown.pump(master)
	go func() { t.done <- cmd.Wait() }()
	return t
}

func (t *term) waitFor(want string) bool {
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(t.shown.String(), want) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

func (t *term) echo() bool {
	tio, err := unix.IoctlGetTermios(int(t.slave.Fd()), ioctlGetTermios)
	if err != nil {
		fatal("read the terminal settings: %v", err)
	}
	return tio.Lflag&unix.ECHO != 0
}

func (t *term) typeLine(line string) {
	if _, err := t.master.WriteString(line + "\n"); err != nil {
		fatal("type on the terminal: %v", err)
	}
}

var exitRE = regexp.MustCompile(`skep-exit=(\d+)`)

func (t *term) finish() int {
	if !t.waitFor("skep-exit=") {
		fatal("skepctl did not exit; the terminal showed %q", t.shown.String())
	}
	m := exitRE.FindStringSubmatch(t.shown.String())
	n, _ := strconv.Atoi(m[1])
	t.cmd.Process.Kill()
	<-t.done
	t.master.Close()
	t.slave.Close()
	return n
}

var pinRE = regexp.MustCompile(`public key:\s+(sha256:[0-9a-f]{64})`)

func (e *env) fingerprint() string {
	r := e.apiLocal("", "tls", "fingerprint")
	m := pinRE.FindStringSubmatch(r.out)
	if r.exit != 0 || m == nil {
		fatal("tls fingerprint: %s", r.out)
	}
	return m[1]
}

// logFile is a process log that the test can count lines in.
type logFile struct{ path string }

func (l logFile) count(substr string) int {
	data, _ := os.ReadFile(l.path)
	return strings.Count(string(data), substr)
}

func startProc(logPath, bin string, args ...string) *exec.Cmd {
	f, err := os.Create(logPath)
	if err != nil {
		fatal("%v", err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		fatal("start %s: %v", bin, err)
	}
	return cmd
}

func waitHealthy() bool {
	c := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, // only to wait for the port; nothing is sent but a health check
	}}}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		resp, err := c.Get(server + "/api/v1/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
	}
	return false
}

func mode(path string) os.FileMode {
	info, err := os.Lstat(path)
	if err != nil {
		return 0
	}
	return info.Mode().Perm()
}

func ownerUID(path string) int {
	info, err := os.Lstat(path)
	if err != nil {
		return -1
	}
	return int(info.Sys().(*syscall.Stat_t).Uid)
}

type sessionsFile struct {
	Current  string `json:"current"`
	Sessions map[string]struct {
		Username string `json:"username"`
		Token    string `json:"token"`
	} `json:"sessions"`
}

func (e *env) sessions() sessionsFile {
	var f sessionsFile
	data, _ := os.ReadFile(filepath.Join(e.ctlDir, "sessions.json"))
	_ = json.Unmarshal(data, &f)
	return f
}

type auditList struct {
	Items []struct {
		ActorName string         `json:"actor_name"`
		Action    string         `json:"action"`
		Outcome   string         `json:"outcome"`
		Details   map[string]any `json:"details"`
	} `json:"items"`
}

func (e *env) audit() auditList {
	r := e.ctlRun("audit", "list", "--limit", "200", "--json")
	var l auditList
	if r.exit != 0 || json.Unmarshal([]byte(r.out), &l) != nil {
		fatal("audit list: %s", r.out)
	}
	return l
}

func (l auditList) count(actor, action, outcome string) int {
	n := 0
	for _, a := range l.Items {
		if (actor == "" || a.ActorName == actor) && a.Action == action && (outcome == "" || a.Outcome == outcome) {
			n++
		}
	}
	return n
}

func main() {
	dir := flag.String("dir", ".", "directory with skep-agent, skep-api, and skepctl")
	flag.Parse()
	if os.Geteuid() == 0 {
		fatal("run this test as an ordinary user")
	}
	binDir, err := filepath.Abs(*dir)
	if err != nil {
		fatal("%v", err)
	}
	*dir = binDir
	scratch, err := os.MkdirTemp("", "skep-skepctl")
	if err != nil {
		fatal("%v", err)
	}
	cleanups = append(cleanups, func() { os.RemoveAll(scratch) })
	e := &env{
		agent: filepath.Join(*dir, "skep-agent"), api: filepath.Join(*dir, "skep-api"), ctl: filepath.Join(*dir, "skepctl"),
		dataDir: filepath.Join(scratch, "data"), ctlDir: filepath.Join(scratch, "home", ".config", "skepctl"),
	}
	uid := os.Getuid()
	fmt.Printf("       %s/%s, uid %d\n", runtime.GOOS, runtime.GOARCH, uid)
	fmt.Printf("       %s", e.ctlRun("version").out)

	step("setup: agent and API in development mode, one administrator")
	if r := e.apiLocal(password+"\n", "admin", "create", "--username", "andy", "--password-stdin"); !check(r.exit == 0, "skep-api admin create andy: %s", short(r.out)) {
		exit(1)
	}
	agent := startProc(filepath.Join(scratch, "agent.log"), e.agent, "-dev", "-data-dir", e.dataDir)
	cleanups = append(cleanups, func() { agent.Process.Kill(); agent.Wait() })
	apiLog := logFile{filepath.Join(scratch, "api.log")}
	api := startProc(apiLog.path, e.api, "-dev", "-data-dir", e.dataDir, "-listen", listen, "-log-format", "json")
	cleanups = append(cleanups, func() { api.Process.Kill(); api.Wait() })
	if !check(waitHealthy(), "skep-api answers on %s", listen) {
		exit(1)
	}
	pin := e.fingerprint()
	fmt.Printf("       skep-api tls fingerprint: %s\n", pin)

	step("first contact without a terminal: refused, the key is shown, nothing stored")
	before := apiLog.count(`"msg":"http request"`)
	r := e.runNoTTY(password+"\n", e.ctl, "login", "--server", listen, "--username", "andy", "--password-stdin")
	check(r.exit != 0, "login fails (exit %d)", r.exit)
	check(strings.Contains(r.out, "--trust-key "+pin), "the error names --trust-key with the pin skep-api prints")
	check(strings.Contains(r.out, "public key:   "+pin), "the certificate's key is shown for comparison")
	_, statErr := os.Stat(filepath.Join(e.ctlDir, "pins.json"))
	check(os.IsNotExist(statErr), "no pin was stored")
	check(apiLog.count(`"msg":"http request"`) == before, "no HTTP request reached skep-api (only a handshake)")

	other := "sha256:" + strings.Repeat("0", 64)
	r = e.runNoTTY(password+"\n", e.ctl, "login", "--server", listen, "--username", "andy", "--password-stdin", "--trust-key", other)
	check(r.exit != 0 && strings.Contains(r.out, "nothing was trusted"), "a --trust-key that does not match is refused: %s", short(r.out))

	step("first contact on a terminal: the key is confirmed, the password typed without echo")
	t := e.onTerminal("login", "--server", listen, "--username", "andy")
	check(t.waitFor("Trust this key? [y/N] "), "skepctl asks on the terminal")
	check(strings.Contains(t.shown.String(), pin), "the terminal shows the pin skep-api prints")
	t.typeLine("y")
	check(t.waitFor("Password for andy at "+server+": "), "then asks for the password")
	time.Sleep(100 * time.Millisecond)
	check(!t.echo(), "echo is off while the password is typed")
	t.typeLine(password)
	check(t.waitFor("Logged in to "+server+" as andy (role admin)."), "logged in")
	code := t.finish()
	check(code == 0, "exit status 0")
	check(!strings.Contains(t.shown.String(), password), "the terminal never showed the password")

	step("the state directory: 0700, files 0600, owned by the user")
	check(mode(e.ctlDir) == 0o700, "state directory mode %04o (want 0700)", mode(e.ctlDir))
	for _, name := range []string{"pins.json", "sessions.json"} {
		p := filepath.Join(e.ctlDir, name)
		check(mode(p) == 0o600, "%s mode %04o (want 0600)", name, mode(p))
		check(ownerUID(p) == uid, "%s owned by uid %d", name, ownerUID(p))
	}
	pinsData, _ := os.ReadFile(filepath.Join(e.ctlDir, "pins.json"))
	check(strings.Contains(string(pinsData), pin), "pins.json holds the confirmed pin")
	sf := e.sessions()
	check(sf.Current == server && sf.Sessions[server].Username == "andy" && len(sf.Sessions[server].Token) >= 40,
		"sessions.json holds a bearer token for andy, and the current server")

	step("read-only commands")
	r = e.ctlRun("system")
	check(r.exit == 0 && strings.Contains(r.out, "Platform:") && strings.Contains(r.out, "freebsd") && strings.Contains(r.out, pin),
		"system (text): platform freebsd, the TLS public key shown")
	fmt.Print(indent(r.out))
	r = e.ctlRun("system", "--json")
	var sys struct {
		NodeID   string `json:"node_id"`
		Platform string `json:"platform"`
	}
	check(r.exit == 0 && json.Unmarshal([]byte(r.out), &sys) == nil && sys.Platform == "freebsd" && sys.NodeID != "",
		"system --json: node %s, platform %s", sys.NodeID, sys.Platform)
	r = e.ctlRun("hardware")
	check(r.exit == 0 && strings.Contains(r.out, "bhyve:") && strings.Contains(r.out, "CPU:") && strings.Contains(r.out, sys.NodeID),
		"hardware of this node (from the system document)")
	fmt.Print(indent(r.out))
	r = e.ctlRun("tasks", "list")
	check(r.exit == 0 && strings.Contains(r.out, "discovery.refresh"), "tasks list shows the startup discovery.refresh task")
	fmt.Print(indent(r.out))
	var tl struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	r = e.ctlRun("tasks", "list", "--json", "--limit", "1")
	if check(r.exit == 0 && json.Unmarshal([]byte(r.out), &tl) == nil && len(tl.Items) == 1, "tasks list --json --limit 1") {
		r = e.ctlRun("tasks", "show", tl.Items[0].ID)
		check(r.exit == 0 && strings.Contains(r.out, "Events:"), "tasks show %s, with its events", tl.Items[0].ID)
	}
	r = e.ctlRun("tasks", "list", "--status", "bogus")
	check(r.exit != 0 && strings.Contains(r.out, "invalid_request"), "a bad status is refused by the server: %s", short(r.out))
	al := e.audit()
	check(al.count("andy", "auth.login", "success") == 1, "audit list: andy's login is recorded")
	for _, a := range al.Items {
		if a.Action == "auth.login" && a.Outcome == "success" {
			check(a.Details["session_kind"] == "bearer", "the login was a bearer session (%v)", a.Details["session_kind"])
		}
	}

	step("a token file readable by others is refused")
	sessPath := filepath.Join(e.ctlDir, "sessions.json")
	os.Chmod(sessPath, 0o644)
	r = e.ctlRun("system")
	check(r.exit != 0 && strings.Contains(r.out, "chmod 600"), "refused with mode 0644: %s", short(r.out))
	os.Chmod(sessPath, 0o600)
	os.Chmod(e.ctlDir, 0o755)
	r = e.ctlRun("system")
	check(r.exit != 0 && strings.Contains(r.out, "chmod 700"), "refused with a 0755 directory: %s", short(r.out))
	os.Chmod(e.ctlDir, 0o700)

	step("a changed key: refused before anything is sent")
	r = e.apiLocal("", "tls", "regenerate")
	check(r.exit == 0, "skep-api tls regenerate")
	api.Process.Signal(syscall.SIGHUP)
	newPin := pin
	for deadline := time.Now().Add(10 * time.Second); newPin == pin && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		newPin = e.fingerprint()
	}
	check(newPin != pin, "the key changed: %s", newPin)
	time.Sleep(500 * time.Millisecond) // let the server load it
	before = apiLog.count(`"msg":"http request"`)
	r = e.ctlRun("system")
	check(r.exit != 0 && strings.Contains(r.out, "has changed") && strings.Contains(r.out, "presented: "+newPin) &&
		strings.Contains(r.out, "pinned:    "+pin), "system refuses and shows both pins")
	fmt.Print(indent(r.out))
	check(apiLog.count(`"msg":"http request"`) == before, "no HTTP request reached skep-api (the token was not sent)")
	check(e.sessions().Sessions[server].Token != "", "the token is kept")
	r = e.runNoTTY(password+"\n", e.ctl, "login", "--username", "andy", "--password-stdin")
	check(r.exit != 0 && strings.Contains(r.out, "has changed"), "login without --trust-key is refused too")
	check(apiLog.count(`"msg":"http request"`) == before, "the password was not sent")
	r = e.runNoTTY(password+"\n", e.ctl, "login", "--username", "andy", "--password-stdin", "--trust-key", newPin)
	check(r.exit == 0 && strings.Contains(r.out, "old: "+pin) && strings.Contains(r.out, "new: "+newPin),
		"login --trust-key with the new pin replaces it: %s", short(r.out))
	check(e.ctlRun("system").exit == 0, "system works with the new key")
	al = e.audit()
	check(al.count("andy", "auth.logout", "success") == 1, "the previous session was ended on the server by the new login")

	step("logout")
	r = e.ctlRun("logout")
	check(r.exit == 0 && strings.Contains(r.out, "Logged out of "+server), "logout: %s", short(r.out))
	_, has := e.sessions().Sessions[server]
	check(!has, "the token is gone from sessions.json")
	r = e.ctlRun("system")
	check(r.exit != 0 && strings.Contains(r.out, "not logged in"), "system after logout: %s", short(r.out))

	step("a session ended on the server: the token is forgotten")
	if r := e.apiLocal(password+"\n", "admin", "create", "--username", "carol", "--password-stdin"); !check(r.exit == 0, "skep-api admin create carol") {
		exit(1)
	}
	r = e.runNoTTY(password+"\n", e.ctl, "login", "--username", "carol", "--password-stdin")
	check(r.exit == 0, "carol logs in")
	r = e.apiLocal("", "admin", "disable", "--username", "carol")
	check(r.exit == 0 && strings.Contains(r.out, `"carol" was disabled`), "skep-api admin disable carol: %s", short(r.out))
	r = e.ctlRun("system")
	check(r.exit != 0 && strings.Contains(r.out, "has ended"), "carol's next command: %s", short(r.out))
	_, has = e.sessions().Sessions[server]
	check(!has, "the ended session's token was removed")

	step("the last administrator: neither disabled nor deleted")
	r = e.apiLocal("", "admin", "disable", "--username", "andy")
	check(r.exit != 0 && strings.Contains(r.out, "only enabled administrator"), "admin disable andy refused: %s", short(r.out))
	r = e.apiLocal("", "admin", "delete", "--username", "andy", "--yes")
	check(r.exit != 0 && strings.Contains(r.out, "only enabled administrator"), "admin delete andy refused: %s", short(r.out))
	r = e.apiLocal("", "admin", "delete", "--username", "carol")
	check(r.exit != 0 && strings.Contains(r.out, "--yes"), "admin delete carol without --yes does nothing: %s", short(r.out))
	r = e.apiLocal("", "admin", "delete", "--username", "carol", "--yes")
	check(r.exit == 0 && strings.Contains(r.out, `"carol" was deleted`), "admin delete carol --yes: %s", short(r.out))
	r = e.runNoTTY(password+"\n", e.ctl, "login", "--username", "andy", "--password-stdin")
	check(r.exit == 0, "andy logs in again")
	al = e.audit()
	local := fmt.Sprintf("local:%d", uid)
	check(al.count(local, "user.update", "success") == 1, "audit: the disable of carol, by %s", local)
	check(al.count(local, "user.update", "denied") == 1, "audit: the refused disable of andy")
	check(al.count(local, "user.delete", "denied") == 1, "audit: the refused delete of andy")
	check(al.count(local, "user.delete", "success") == 1, "audit: the delete of carol")
	check(al.count(local, "session.revoke", "success") == 1, "audit: carol's session revoked by the disable")
	step("an operator-supplied certificate: chain verification, renewals with a new key")
	api.Process.Signal(syscall.SIGTERM)
	api.Wait()
	ca := newCA()
	caFile := filepath.Join(scratch, "operator-ca.pem")
	writePEM(caFile, "CERTIFICATE", ca.cert.Raw)
	opCert, opKey := filepath.Join(scratch, "op-cert.pem"), filepath.Join(scratch, "op-key.pem")
	first := ca.issue(opCert, opKey, 10)
	cfgFile := filepath.Join(scratch, "api.json")
	cfg, _ := json.Marshal(map[string]any{"tls": map[string]string{"cert_file": opCert, "key_file": opKey}})
	os.WriteFile(cfgFile, cfg, 0o600)
	api = startProc(apiLog.path+".op", e.api, "-dev", "-config", cfgFile, "-data-dir", e.dataDir, "-listen", listen, "-log-format", "json")
	cleanups = append(cleanups, func() { api.Process.Kill(); api.Wait() })
	if !check(waitHealthy(), "skep-api answers with the operator certificate") {
		exit(1)
	}
	r = e.ctlRun("system")
	check(r.exit != 0 && strings.Contains(r.out, "has changed") && strings.Contains(r.out, "--ca-file"),
		"the pinned self-signed key no longer matches; the error points to --ca-file")
	r = e.runNoTTY(password+"\n", e.ctl, "login", "--username", "andy", "--password-stdin", "--system-roots")
	check(r.exit != 0 && strings.Contains(r.out, "does not verify against the system roots"), "--system-roots refuses the private CA: %s", short(r.out))
	r = e.runNoTTY(password+"\n", e.ctl, "login", "--username", "andy", "--password-stdin", "--ca-file", caFile)
	check(r.exit == 0 && strings.Contains(r.out, "from the pinned key") && strings.Contains(r.out, "Logged in"),
		"login --ca-file switches from the pin to chain verification: %s", short(r.out))
	pinsData, _ = os.ReadFile(filepath.Join(e.ctlDir, "pins.json"))
	check(strings.Contains(string(pinsData), `"mode": "ca"`) && strings.Contains(string(pinsData), caFile), "pins.json holds mode ca and the CA file")
	check(e.ctlRun("system").exit == 0, "system works")
	second := ca.issue(opCert, opKey, 11)
	check(spkiPin(second) != spkiPin(first), "the operator renews the certificate with a new key")
	api.Process.Signal(syscall.SIGHUP)
	time.Sleep(time.Second)
	r = e.ctlRun("system")
	check(r.exit == 0 && strings.Contains(r.out, spkiPin(second)), "system works after the renewal, without a new login; the server shows the new key")
	otherCA := newCA()
	otherCA.issue(opCert, opKey, 12)
	api.Process.Signal(syscall.SIGHUP)
	time.Sleep(time.Second)
	before = apiLog.count(`"msg":"http request"`) + logFile{apiLog.path + ".op"}.count(`"msg":"http request"`)
	r = e.ctlRun("system")
	check(r.exit != 0 && strings.Contains(r.out, "not trusted"), "a certificate from another CA is refused: %s", short(r.out))
	check(apiLog.count(`"msg":"http request"`)+logFile{apiLog.path + ".op"}.count(`"msg":"http request"`) == before, "no HTTP request reached skep-api")
	ca.issue(opCert, opKey, 13)
	api.Process.Signal(syscall.SIGHUP)
	time.Sleep(time.Second)
	r = e.ctlRun("logout")
	check(r.exit == 0, "logout")

	// A clean stop: afterwards only the scratch directory is left to
	// clean up.
	api.Process.Signal(syscall.SIGTERM)
	agent.Process.Signal(syscall.SIGTERM)
	api.Wait()
	agent.Wait()
	cleanups = cleanups[:1]

	fmt.Println()
	if failed > 0 {
		fmt.Printf("FAIL   %d checks failed\n", failed)
		exit(1)
	}
	fmt.Println("PASS")
	exit(0)
}

func indent(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("       | " + line + "\n")
	}
	return b.String()
}

// testCA is an operator's CA.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCA() *testCA {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Skep test operator CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		fatal("create the CA: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert: cert, key: key}
}

// issue writes a certificate for 127.0.0.1 with a new key, as an operator
// renewing it would.
func (ca *testCA) issue(certFile, keyFile string, serial int64) *x509.Certificate {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "hearst"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		fatal("issue a certificate: %v", err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	writePEM(keyFile, "PRIVATE KEY", keyDER)
	writePEM(certFile, "CERTIFICATE", der)
	cert, _ := x509.ParseCertificate(der)
	return cert
}

func writePEM(path, typ string, der []byte) {
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		fatal("write %s: %v", path, err)
	}
}

func spkiPin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:])
}
