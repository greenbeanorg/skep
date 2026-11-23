package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Applier makes a plan's changes on the host under Root.
type Applier struct {
	Root   string
	Runner Runner
	Now    func() time.Time
	// Chown sets a file's owner; os.Lchown by default. Tests replace it
	// because they do not run as root.
	Chown func(path string, uid, gid int) error
	// Out receives one line per action.
	Out         io.Writer
	SkepVersion string
	// Command names the run in its journal: "apply" (default) or "remove".
	Command string
	// Sleep and AdminWait pace the wait for skep_api's pid file and
	// api.db before the first administrator is created (default
	// time.Sleep and DefaultAdminWait).
	Sleep     func(time.Duration)
	AdminWait time.Duration
}

// DefaultAdminWait is how long apply waits for skep_api's pid file and
// api.db before it creates the first administrator.
const DefaultAdminWait = 30 * time.Second

// AdminCreateError is a failed "skep-api admin create". The run went on to
// its end and the journal is complete, so the same apply can simply be run
// again: it plans only the administrator. Alone it means the run changed
// nothing else; after other changes it is wrapped in an *Incomplete.
type AdminCreateError struct {
	Username string
	Journal  string
	Err      error
}

func (e *AdminCreateError) Error() string {
	return fmt.Sprintf("creating the administrator %s failed: %v (journal %s, complete)", e.Username, e.Err, e.Journal)
}

func (e *AdminCreateError) Unwrap() error { return e.Err }

// skippedError is an action not made because the host changed after the
// plan.
type skippedError struct{ reason string }

func (e *skippedError) Error() string { return e.reason }

func (a *Applier) chown(path string, uid, gid int) error {
	if a.Chown != nil {
		return a.Chown(path, uid, gid)
	}
	return os.Lchown(path, uid, gid)
}

func (a *Applier) printf(format string, args ...any) {
	if a.Out != nil {
		fmt.Fprintf(a.Out, format, args...)
	}
}

// ErrNothingToDo is returned by Apply when the plan has no action.
var ErrNothingToDo = errors.New("nothing to do: the host is already as planned")

// Apply makes the plan's actions, in order, after backing up every file
// it changes. It refuses a plan with a refusal. On a failure it stops and
// leaves an incomplete journal, which makes the next plan refuse until
// it is rolled back.
func (a *Applier) Apply(ctx context.Context, p Plan) (*Journal, error) {
	if p.Refused() {
		return nil, errors.New("the plan has refusals; nothing was changed")
	}
	acts := p.Actions()
	if len(acts) == 0 {
		return nil, ErrNothingToDo
	}
	cmd := a.Command
	if cmd == "" {
		cmd = "apply"
	}
	j, err := a.newJournal(cmd, acts)
	if err != nil {
		return nil, fmt.Errorf("could not create the journal: %w; no action ran, so nothing was changed", err)
	}
	backedUp := map[string]bool{}
	var adminErr *AdminCreateError
	// What each entry did to the host, for the message if the run fails.
	effects := make([]effect, len(j.Entries))
	for i := range j.Entries {
		if err := ctx.Err(); err != nil {
			return j, a.cancelled(j, i, effects, err)
		}
		e := &j.Entries[i]
		act := e.Action
		act.Content = acts[i].Content
		if act.File != "" && (act.Type == ActSysrc || act.Type == ActWrite || (act.Type == ActRemovePath && isRegular(hostPath(a.Root, act.File)))) {
			e.File = act.File
			if !backedUp[act.File] {
				if err := a.backup(j, e); err != nil {
					return j, a.fail(j, i, effects, err)
				}
				backedUp[act.File] = true
			}
		}
		a.printf("  %s\n", act)
		before := a.snapshot(act, e.File)
		noop, err := a.do(ctx, act)
		a.recordAfter(e)
		var skip *skippedError
		switch {
		case errors.As(err, &skip):
			a.printf("    skipped: %s\n", skip.reason)
			e.Skipped, e.Error = true, skip.reason
		case err != nil && act.Type == ActAdminCreate:
			// The administrator is the one action a second apply can
			// simply retry; the run itself is not left unfinished.
			e.Error = err.Error()
			adminErr = &AdminCreateError{Username: act.Username, Journal: j.ID, Err: err}
			var killed *interruptedError
			if errors.As(err, &killed) {
				effects[i] = effectUncertain
			}
		case err != nil:
			effects[i] = a.failedEffect(act, e.File, before)
			return j, a.fail(j, i, effects, err)
		default:
			e.Done = true
			if !noop && a.changed(act, e.File, before) {
				effects[i] = effectChanged
			}
		}
		if err := j.save(a.Root); err != nil {
			return j, a.saveFailed(j, effects, err)
		}
	}
	now := a.Now().UTC()
	j.Complete, j.FinishedAt = true, &now
	if err := j.save(a.Root); err != nil {
		j.Complete, j.FinishedAt = false, nil
		return j, a.saveFailed(j, effects, err)
	}
	if adminErr != nil {
		if inc := adminIncomplete(j, effects, adminErr); inc != nil {
			return j, inc
		}
		return j, adminErr
	}
	return j, nil
}

// newJournal creates the journal directory and an incomplete journal.
func (a *Applier) newJournal(command string, acts []Action) (*Journal, error) {
	now := a.Now().UTC()
	j := &Journal{Format: JournalFormat, ID: now.Format("20060102T150405Z"), Command: command,
		SkepVersion: a.SkepVersion, StartedAt: now}
	for _, act := range acts {
		j.Entries = append(j.Entries, Entry{Action: act})
	}
	parent := filepath.Join(a.Root, journalRoot)
	if err := os.MkdirAll(filepath.Dir(parent), 0o755); err != nil {
		return nil, err
	}
	if err := os.Mkdir(parent, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	if err := os.Mkdir(JournalDir(a.Root, j.ID), 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("journal %s exists; run again in a second", j.ID)
		}
		return nil, err
	}
	// From here the directory exists, and the next plan refuses on it as
	// an unfinished run until it is rolled back.
	if err := os.Mkdir(filepath.Join(JournalDir(a.Root, j.ID), "files"), 0o700); err != nil {
		return nil, fmt.Errorf("%w (clear the partial journal before running %s again: skep-setup rollback %s)", err, command, j.ID)
	}
	if err := j.save(a.Root); err != nil {
		return nil, fmt.Errorf("%w (clear the partial journal before running %s again: skep-setup rollback %s)", err, command, j.ID)
	}
	return j, nil
}

// fail records a failed action. With nothing changed it is a plain error
// (exit 3); after any change, or when the failed action may have changed
// something, it is an *Incomplete.
func (a *Applier) fail(j *Journal, i int, effects []effect, err error) error {
	j.Entries[i].Error = err.Error()
	serr := j.save(a.Root)
	if anyEffect(effects) {
		inc := newIncomplete(j, effects, fmt.Sprintf("%s failed: %v", j.Entries[i].Action, err))
		if serr != nil {
			inc.Problem += fmt.Sprintf(" (and the journal could not be saved: %v, so it may not list the last change)", serr)
		}
		inc.Err = err
		return inc
	}
	if serr != nil {
		return fmt.Errorf("%w (and the journal could not be saved: %v)", err, serr)
	}
	return fmt.Errorf("%s failed: %w; nothing was changed. Clear this run before running %s again: skep-setup rollback %s",
		j.Entries[i].Action, err, j.Command, j.ID)
}

// cancelled records a run cancelled between actions: action i never
// started, so the message names the point of interruption and does not
// present that action as having failed.
func (a *Applier) cancelled(j *Journal, i int, effects []effect, cause error) error {
	point := fmt.Sprintf("cancelled (%v) before %s started; that action did not run", cause, j.Entries[i].Action)
	j.Entries[i].Error = point
	serr := j.save(a.Root)
	if anyEffect(effects) {
		inc := newIncomplete(j, effects, point)
		if serr != nil {
			inc.Problem += fmt.Sprintf(" (and the journal could not be saved: %v, so it may not list the last change)", serr)
		}
		inc.Err = cause
		return inc
	}
	if serr != nil {
		return fmt.Errorf("%s; nothing was changed (and the journal could not be saved: %v): %w", point, serr, cause)
	}
	return fmt.Errorf("%s; nothing was changed. Clear this run before running %s again: skep-setup rollback %s: %w",
		point, j.Command, j.ID, cause)
}

// saveFailed is a journal that could not be saved after an action.
func (a *Applier) saveFailed(j *Journal, effects []effect, err error) error {
	if !anyEffect(effects) {
		return fmt.Errorf("the journal could not be saved: %w; nothing was changed. Clear this run before running %s again: skep-setup rollback %s",
			err, j.Command, j.ID)
	}
	inc := newIncomplete(j, effects, fmt.Sprintf("the journal could not be saved (%v), so it may not list the last change", err))
	inc.Err = err
	return inc
}

// backupName is the copy's name inside the journal's files/ directory.
func backupName(path string) string {
	return strings.ReplaceAll(strings.TrimPrefix(path, "/"), "/", "_")
}

// backup copies e.File into the journal before its first change in this
// run, with its SHA-256.
func (a *Applier) backup(j *Journal, e *Entry) error {
	full := hostPath(a.Root, e.File)
	b, err := os.ReadFile(full)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	name := filepath.Join("files", backupName(e.File))
	if err := writeAtomic(filepath.Join(JournalDir(a.Root, j.ID), name), b, 0o600); err != nil {
		return err
	}
	e.Backup, e.BeforeSHA256 = name, sha256Hex(b)
	return nil
}

func (a *Applier) recordAfter(e *Entry) {
	if e.File == "" {
		return
	}
	if sum, err := fileSHA256(hostPath(a.Root, e.File)); err == nil {
		e.AfterSHA256 = sum
	}
}

// do runs one action.
// do runs one action. noop reports that it succeeded without changing
// anything (the service was already in that state).
func (a *Applier) do(ctx context.Context, act Action) (noop bool, err error) {
	switch act.Type {
	case ActSysrc:
		return false, a.sysrc(ctx, act)
	case ActWrite:
		return false, a.writeNew(act)
	case ActMkdir:
		return false, a.mkdir(act)
	case ActServiceStart:
		if _, err := a.Runner.Run(ctx, Cmd{Name: cmdService, Args: []string{act.Service, "status"}}); err == nil {
			a.printf("    %s is running already\n", act.Service)
			return true, nil
		}
		_, err := a.Runner.Run(ctx, Cmd{Name: cmdService, Args: []string{act.Service, "start"}})
		return false, err
	case ActAdminCreate:
		return false, a.adminCreate(ctx, act.Username)
	case ActServiceStop:
		return stopIfRunning(ctx, a.Runner, act.Service, a.printf)
	case ActRemovePath:
		return false, a.removePath(act.File)
	case ActPwDelete:
		return false, a.pwDelete(ctx, act.Name)
	}
	return false, fmt.Errorf("unknown action %q", act.Type)
}

// stopService stops an rc.d service that is running; one that is not
// ("service NAME status" fails) is left as it is, since "service NAME
// stop" fails on it.
func stopService(ctx context.Context, r Runner, svc string, printf func(string, ...any)) error {
	_, err := stopIfRunning(ctx, r, svc, printf)
	return err
}

// stopIfRunning is stopService, also reporting that the service was not
// running, so nothing changed.
func stopIfRunning(ctx context.Context, r Runner, svc string, printf func(string, ...any)) (noop bool, err error) {
	if _, err := r.Run(ctx, Cmd{Name: cmdService, Args: []string{svc, "status"}}); err != nil {
		printf("    %s is not running\n", svc)
		return true, nil
	}
	_, err = r.Run(ctx, Cmd{Name: cmdService, Args: []string{svc, "stop"}})
	var killed *interruptedError
	if errors.As(markInterrupted(ctx, err), &killed) {
		return false, fmt.Errorf("the stop of %s was killed and may have taken effect: %w", svc, err)
	}
	return false, err
}

// adminCreate runs "skep-api admin create" as the skep account (its UID
// and primary GID from the root's /etc/passwd), never as root, once
// skep_api has written its pid file and created api.db: run earlier, or as
// root, it could create the API's database owned by root (ADR 0012
// amendment, build decision 8).
func (a *Applier) adminCreate(ctx context.Context, username string) error {
	cred, err := skepCredential(a.Root)
	if err != nil {
		return err
	}
	db, err := APIDBPath(a.Root)
	if err != nil {
		return fmt.Errorf("skep-api's configuration: %w", err)
	}
	wait := a.AdminWait
	if wait <= 0 {
		wait = DefaultAdminWait
	}
	sleep := a.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	pid := hostPath(a.Root, apiPID)
	for waited := time.Duration(0); ; waited += time.Second {
		b, _ := os.ReadFile(pid)
		if strings.TrimSpace(string(b)) != "" && isRegular(db) {
			break
		}
		if waited >= wait {
			return fmt.Errorf("skep_api has not written %s and created %s within %s; is it running (service skep_api status)?",
				apiPID, strings.TrimPrefix(db, strings.TrimSuffix(a.Root, "/")), wait)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		sleep(time.Second)
	}
	svc, err := apiServiceArgs(a.Root)
	if err != nil {
		return err
	}
	_, err = a.Runner.Run(ctx, Cmd{Name: hostPath(a.Root, apiBin), Args: append([]string{"admin", "create", "--username", username}, svc...),
		Interactive: true, As: cred})
	return markInterrupted(ctx, err)
}

// skepCredential is the skep account's UID and primary GID from the
// root's /etc/passwd; never root.
func skepCredential(root string) (*Credential, error) {
	users, err := ReadAccounts(hostPath(root, "/etc/passwd"))
	if err != nil {
		return nil, err
	}
	u, ok := findAccount(users, skepAccount)
	if !ok {
		return nil, errors.New("no skep user in /etc/passwd")
	}
	uid, err1 := strconv.ParseUint(u.ID, 10, 32)
	gid, err2 := strconv.ParseUint(u.GID, 10, 32)
	if err1 != nil || err2 != nil || uid == 0 || gid == 0 {
		return nil, fmt.Errorf("the skep user has UID %q and GID %q; refusing to run skep-api as it", u.ID, u.GID)
	}
	return &Credential{UID: uint32(uid), GID: uint32(gid)}, nil
}

func isRegular(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.Mode().IsRegular()
}

// removePath deletes Skep's own data only: something under /var/db/skep
// other than setup's journals, or agent.json or api.json.
func (a *Applier) removePath(p string) error {
	clean := filepath.Clean(p)
	inData := strings.HasPrefix(clean, dataRoot+"/") && strings.Count(clean, "/") == strings.Count(dataRoot, "/")+1 &&
		clean != dataRoot+"/setup"
	if !inData && clean != agentJSONPath && clean != apiJSONPath {
		return fmt.Errorf("refusing to delete %s: not Skep's data", p)
	}
	return os.RemoveAll(hostPath(a.Root, clean))
}

// pwDelete removes the skep user and its group.
func (a *Applier) pwDelete(ctx context.Context, name string) error {
	if name != skepAccount {
		return fmt.Errorf("refusing to delete the account %q", name)
	}
	pw := func(args ...string) Cmd {
		if a.Root != "/" {
			args = append([]string{"-R", a.Root}, args...)
		}
		return Cmd{Name: cmdPw, Args: args}
	}
	users, _ := ReadAccounts(hostPath(a.Root, "/etc/passwd"))
	if _, ok := findAccount(users, name); ok {
		if _, err := a.Runner.Run(ctx, pw("userdel", name)); err != nil {
			return err
		}
	}
	groups, _ := ReadAccounts(hostPath(a.Root, "/etc/group"))
	if _, ok := findAccount(groups, name); ok {
		if _, err := a.Runner.Run(ctx, pw("groupdel", name)); err != nil {
			return err
		}
	}
	return nil
}

// sysrc runs sysrc(8) on the file and checks the result by reading it
// back: a value sysrc did not write as asked is an error.
func (a *Applier) sysrc(ctx context.Context, act Action) error {
	full := hostPath(a.Root, act.File)
	if err := unchangedSincePlan(full, act); err != nil {
		return err
	}
	if SysrcCanEdit(act.Name) {
		args := []string{"-f", full, act.Name + act.Op + act.Value}
		if act.Op == "-x" {
			args = []string{"-f", full, "-x", act.Name}
		}
		if _, err := a.Runner.Run(ctx, Cmd{Name: cmdSysrc, Args: args}); err != nil {
			return err
		}
	} else if err := a.editTunable(full, act); err != nil {
		return err
	}
	v, err := ReadShVars(full)
	if err != nil {
		return err
	}
	got, ok := v.Vals[act.Name]
	switch {
	case act.Op == "-x":
		if ok {
			return fmt.Errorf("%s: %s is still set after sysrc -x", act.File, act.Name)
		}
		return nil
	case act.Op == "-=":
		for _, w := range strings.Fields(act.Value) {
			if slices.Contains(strings.Fields(got), w) {
				return fmt.Errorf("%s: %s is %q after sysrc, still has %q", act.File, act.Name, got, w)
			}
		}
		return nil
	case !ok:
		return fmt.Errorf("%s: %s is not set after sysrc", act.File, act.Name)
	case act.Op == "=" && got != act.Value:
		return fmt.Errorf("%s: %s is %q after sysrc, want %q", act.File, act.Name, got, act.Value)
	case act.Op == "+=":
		have := strings.Fields(got)
		for _, w := range strings.Fields(act.Value) {
			if !slices.Contains(have, w) {
				return fmt.Errorf("%s: %s is %q after sysrc, lacks %q", act.File, act.Name, got, w)
			}
		}
	}
	return nil
}

// unchangedSincePlan re-reads the variable just before a sysrc "=" or
// "-x": an "=" is made only while the variable is as the plan saw it
// (unset, or Expect), a "-x" only while it still has Expect. Anything
// else means someone changed the file meanwhile, and the action is
// skipped rather than overwriting their change. "+=" and "-=" change only
// their own words and are not checked.
func unchangedSincePlan(full string, act Action) error {
	if act.Op != "=" && act.Op != "-x" {
		return nil
	}
	v, err := ReadShVars(full)
	if err != nil {
		return err
	}
	cur, set := v.Vals[act.Name]
	switch {
	case act.Op == "-x" && !set:
		return &skippedError{act.Name + " is gone already"}
	case act.Expect == nil && set:
		return &skippedError{fmt.Sprintf("%s was set to %q after the plan; left alone", act.Name, cur)}
	case act.Expect != nil && !set:
		return &skippedError{fmt.Sprintf("%s was removed after the plan; left alone", act.Name)}
	case act.Expect != nil && cur != *act.Expect:
		return &skippedError{fmt.Sprintf("%s changed to %q after the plan (was %q); left alone", act.Name, cur, *act.Expect)}
	}
	return nil
}

// shellNameRE is what sysrc(8) accepts as a variable name.
var shellNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// tunableNameRE is a loader tunable such as kern.racct.enable.
var tunableNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z0-9_]+)+$`)

// SysrcCanEdit reports whether sysrc(8) can set or remove the variable.
// It accepts shell variable names only: on FreeBSD 15.1, "sysrc -f
// /boot/loader.conf kern.racct.enable=1" (and -n, -c, -x on such a name)
// fails with "name contains characters not allowed in shell". Setup edits
// those loader.conf lines itself (editTunable).
func SysrcCanEdit(name string) bool { return shellNameRE.MatchString(name) }

// editTunable does what sysrc would do for a loader tunable whose name
// sysrc refuses: "=" appends name="value" to a file that does not set it,
// "-x" deletes the lines that assign it. Only plain values are written,
// and the file is replaced atomically with its mode and owner, the
// directory synced, as replaceFile does; a file that is not regular (a
// symlink, for one) is refused. The caller reads the result back.
func (a *Applier) editTunable(full string, act Action) error {
	if !tunableNameRE.MatchString(act.Name) {
		return fmt.Errorf("%s: %q is neither a shell variable nor a loader tunable name", act.File, act.Name)
	}
	// Lstat, as replaceFile: a symlink or other non-regular file is
	// refused, never followed or replaced.
	st, err := os.Lstat(full)
	missing := errors.Is(err, fs.ErrNotExist)
	if err != nil && !missing {
		return err
	}
	if !missing && !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file; not replaced", full)
	}
	var b []byte
	if !missing {
		if b, err = os.ReadFile(full); err != nil {
			return err
		}
	}
	var lines []string
	if len(b) > 0 {
		lines = strings.SplitAfter(string(b), "\n")
		if lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
	}
	assigns := func(line string) bool {
		m := assignRE.FindStringSubmatch(strings.TrimSpace(line))
		return m != nil && m[1] == act.Name
	}
	var out []string
	switch act.Op {
	case "=":
		if strings.ContainsAny(act.Value, "\"$`\\\n") {
			return fmt.Errorf("%s: refusing to write %s with the value %q", act.File, act.Name, act.Value)
		}
		for _, l := range lines {
			if assigns(l) {
				return fmt.Errorf("%s: %s is set already; left alone", act.File, act.Name)
			}
		}
		out = lines
		if n := len(out); n > 0 && !strings.HasSuffix(out[n-1], "\n") {
			out[n-1] += "\n"
		}
		out = append(out, act.Name+`="`+act.Value+`"`+"\n")
	case "-x":
		if missing {
			return nil
		}
		for _, l := range lines {
			if !assigns(l) {
				out = append(out, l)
			}
		}
	default:
		return fmt.Errorf("%s: %s %s is not supported for a loader tunable", act.File, act.Op, act.Name)
	}
	mode, uid, gid := fs.FileMode(0o644), 0, 0
	if !missing {
		sys, ok := st.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("%s: no owner information", full)
		}
		mode, uid, gid = st.Mode().Perm(), int(sys.Uid), int(sys.Gid)
	} else if uid, gid, err = a.ids("root", "wheel"); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".skep-setup-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	_, werr := tmp.WriteString(strings.Join(out, ""))
	if werr == nil {
		werr = tmp.Chmod(mode)
	}
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = a.chown(name, uid, gid)
	}
	if werr != nil {
		return werr
	}
	if err := os.Rename(name, full); err != nil {
		return err
	}
	return syncDir(filepath.Dir(full))
}

// ids resolves an owner and group through the root's own account files.
func (a *Applier) ids(owner, group string) (int, int, error) {
	users, err := ReadAccounts(hostPath(a.Root, "/etc/passwd"))
	if err != nil {
		return 0, 0, err
	}
	groups, err := ReadAccounts(hostPath(a.Root, "/etc/group"))
	if err != nil {
		return 0, 0, err
	}
	u, ok := findAccount(users, owner)
	if !ok {
		return 0, 0, fmt.Errorf("no user %q", owner)
	}
	g, ok := findAccount(groups, group)
	if !ok {
		return 0, 0, fmt.Errorf("no group %q", group)
	}
	uid, err1 := strconv.Atoi(u.ID)
	gid, err2 := strconv.Atoi(g.ID)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("non-numeric ID for %s:%s", owner, group)
	}
	return uid, gid, nil
}

// writeNew creates a file that must not exist: written to a temporary
// file with its mode and owner, then linked into place, which fails if
// anything appeared there meanwhile.
func (a *Applier) writeNew(act Action) error {
	full := hostPath(a.Root, act.File)
	uid, gid, err := a.ids(act.Owner, act.Group)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".skep-setup-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	_, werr := tmp.Write(act.Content)
	if werr == nil {
		werr = tmp.Chmod(act.Mode)
	}
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = a.chown(name, uid, gid)
	}
	if werr != nil {
		return werr
	}
	if err := os.Link(name, full); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s appeared while setup ran; left alone", act.File)
		}
		return err
	}
	return nil
}

func (a *Applier) mkdir(act Action) error {
	full := hostPath(a.Root, act.File)
	uid, gid, err := a.ids(act.Owner, act.Group)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(full, act.Mode); err != nil {
		return err
	}
	if err := os.Chmod(full, act.Mode); err != nil {
		return err
	}
	return a.chown(full, uid, gid)
}

// RollbackTarget returns the run Rollback(root, id) would undo: the run
// id, or with an empty id the newest run not rolled back.
func RollbackTarget(root, id string) (Journal, error) {
	js, err := LoadJournals(root)
	if err != nil {
		return Journal{}, err
	}
	for i := len(js) - 1; i >= 0; i-- {
		if (id == "" && js[i].RolledBack == nil) || js[i].ID == id {
			if js[i].RolledBack != nil {
				return Journal{}, fmt.Errorf("%s was rolled back at %s", js[i].ID, js[i].RolledBack.Format(time.RFC3339))
			}
			return js[i], nil
		}
	}
	if id == "" {
		return Journal{}, errors.New("no run to roll back")
	}
	return Journal{}, fmt.Errorf("no journal %s", id)
}

// rollbackUnreadable clears a run without a readable journal.json: setup
// cannot tell what it changed and undoes nothing. A damaged journal.json
// is kept beside the new one as journal.json.unreadable.
func rollbackUnreadable(root string, j *Journal, now time.Time, r RollbackReport) (RollbackReport, error) {
	dir := JournalDir(root, j.ID)
	p := filepath.Join(dir, "journal.json")
	if exists(p) {
		if err := os.Rename(p, p+".unreadable"); err != nil {
			return r, err
		}
	}
	t := now.UTC()
	cleared := Journal{Format: JournalFormat, ID: j.ID, Command: "unknown", StartedAt: t, RolledBack: &t}
	r.Kept = append(r.Kept, fmt.Sprintf("run %s had no readable journal (%s): setup cannot tell what it changed and undid nothing; any file copies it made are in %s",
		j.ID, j.Unreadable, filepath.Join(dir, "files")))
	return r, cleared.save(root)
}

// replaceFile replaces an existing regular file with data through a
// temporary file in the same directory, keeping the file's owner and
// mode, and syncs the file and the directory.
func replaceFile(path string, data []byte) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file; not replaced", path)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: no owner information", path)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".skep-setup-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Chown(int(sys.Uid), int(sys.Gid))
	}
	if werr == nil {
		werr = tmp.Chmod(st.Mode().Perm())
	}
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir makes a rename in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// RollbackReport says what a rollback did and did not undo.
type RollbackReport struct {
	ID       string
	Restored []string // files put back as they were
	Removed  []string // files and directories setup had created
	Kept     []string // left alone, with the reason
}

// Rollback undoes one run (apply or remove): every file it changed is put
// back only if it is still byte-identical to what the run left; a file
// changed since is reported, not overwritten. It never stops a service,
// deletes an administrator, unloads a module, or touches an interface.
// An empty id means the newest run not rolled back.
func Rollback(root, id string, now time.Time) (RollbackReport, error) {
	jj, err := RollbackTarget(root, id)
	if err != nil {
		return RollbackReport{}, err
	}
	j := &jj
	r := RollbackReport{ID: j.ID}
	if j.Unreadable != "" {
		return rollbackUnreadable(root, j, now, r)
	}
	type fileState struct {
		first, last *Entry
	}
	files := map[string]*fileState{}
	var order []string
	for i := range j.Entries {
		e := &j.Entries[i]
		if e.File == "" || e.AfterSHA256 == "" && e.BeforeSHA256 == "" && !e.Done {
			continue
		}
		st, ok := files[e.File]
		if !ok {
			st = &fileState{first: e}
			files[e.File] = st
			order = append(order, e.File)
		}
		st.last = e
	}
	for _, path := range order {
		st := files[path]
		full := hostPath(root, path)
		if st.last.Action.Type == ActRemovePath {
			r.Kept = append(r.Kept, path+": deleted by remove; a copy is in "+filepath.Join(JournalDir(root, j.ID), st.first.Backup))
			continue
		}
		cur, err := fileSHA256(full)
		if err != nil {
			return r, err
		}
		before := st.first.BeforeSHA256
		switch {
		case cur == before:
			r.Kept = append(r.Kept, path+": already as before")
		case cur != st.last.AfterSHA256:
			r.Kept = append(r.Kept, path+": changed since this run; left alone")
		case before == "":
			if err := os.Remove(full); err != nil {
				return r, err
			}
			r.Removed = append(r.Removed, path)
		default:
			b, err := os.ReadFile(filepath.Join(JournalDir(root, j.ID), st.first.Backup))
			if err != nil {
				return r, err
			}
			if sha256Hex(b) != before {
				return r, fmt.Errorf("the backup of %s does not match its recorded SHA-256; nothing restored for it", path)
			}
			// A new file renamed into place with the current owner and
			// mode, so a crash leaves the old or the new content, never
			// a part.
			if err := replaceFile(full, b); err != nil {
				// The rename may have happened before a failed directory
				// sync: report what is on disk.
				if sum, herr := fileSHA256(full); herr == nil && sum == before {
					r.Restored = append(r.Restored, path)
				}
				return r, err
			}
			r.Restored = append(r.Restored, path)
		}
	}
	for i := len(j.Entries) - 1; i >= 0; i-- {
		e := j.Entries[i]
		switch {
		case !e.Done:
		case e.Action.Type == ActMkdir:
			if err := os.Remove(hostPath(root, e.Action.File)); err != nil {
				r.Kept = append(r.Kept, e.Action.File+": not empty or gone; left alone")
			} else {
				r.Removed = append(r.Removed, e.Action.File)
			}
		case e.Action.Type == ActServiceStart:
			r.Kept = append(r.Kept, e.Action.Service+": rollback does not stop services")
		case e.Action.Type == ActAdminCreate:
			r.Kept = append(r.Kept, "administrator "+e.Action.Username+": rollback does not delete users")
		case e.Action.Type == ActServiceStop:
			r.Kept = append(r.Kept, e.Action.Service+": rollback does not start services again")
		case e.Action.Type == ActRemovePath && e.File == "":
			r.Kept = append(r.Kept, e.Action.File+": deleted data cannot be restored")
		case e.Action.Type == ActPwDelete:
			r.Kept = append(r.Kept, "the skep account: rollback does not create it again (reinstall the package)")
		}
	}
	t := now.UTC()
	j.RolledBack = &t
	return r, j.save(root)
}
