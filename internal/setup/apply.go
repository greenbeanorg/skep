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
}

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
		return nil, err
	}
	backedUp := map[string]bool{}
	for i := range j.Entries {
		if err := ctx.Err(); err != nil {
			return j, a.fail(j, i, err)
		}
		e := &j.Entries[i]
		act := e.Action
		act.Content = acts[i].Content
		if act.File != "" && (act.Type == ActSysrc || act.Type == ActWrite || (act.Type == ActRemovePath && isRegular(hostPath(a.Root, act.File)))) {
			e.File = act.File
			if !backedUp[act.File] {
				if err := a.backup(j, e); err != nil {
					return j, a.fail(j, i, err)
				}
				backedUp[act.File] = true
			}
		}
		a.printf("  %s\n", act)
		if err := a.do(ctx, act); err != nil {
			a.recordAfter(e)
			return j, a.fail(j, i, err)
		}
		a.recordAfter(e)
		e.Done = true
		if err := j.save(a.Root); err != nil {
			return j, err
		}
	}
	now := a.Now().UTC()
	j.Complete, j.FinishedAt = true, &now
	return j, j.save(a.Root)
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
	if err := os.Mkdir(filepath.Join(JournalDir(a.Root, j.ID), "files"), 0o700); err != nil {
		return nil, err
	}
	return j, j.save(a.Root)
}

func (a *Applier) fail(j *Journal, i int, err error) error {
	j.Entries[i].Error = err.Error()
	if serr := j.save(a.Root); serr != nil {
		return fmt.Errorf("%w (and the journal could not be saved: %v)", err, serr)
	}
	return fmt.Errorf("%s failed: %w; nothing after it was done. Undo with: skep-setup rollback %s", j.Entries[i].Action, err, j.ID)
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
func (a *Applier) do(ctx context.Context, act Action) error {
	switch act.Type {
	case ActSysrc:
		return a.sysrc(ctx, act)
	case ActWrite:
		return a.writeNew(act)
	case ActMkdir:
		return a.mkdir(act)
	case ActServiceStart:
		if _, err := a.Runner.Run(ctx, Cmd{Name: "service", Args: []string{act.Service, "status"}}); err == nil {
			a.printf("    %s is running already\n", act.Service)
			return nil
		}
		_, err := a.Runner.Run(ctx, Cmd{Name: "service", Args: []string{act.Service, "start"}})
		return err
	case ActAdminCreate:
		_, err := a.Runner.Run(ctx, Cmd{Name: hostPath(a.Root, apiBin), Args: []string{"admin", "create", "--username", act.Username}, Interactive: true})
		return err
	case ActServiceStop:
		if _, err := a.Runner.Run(ctx, Cmd{Name: "service", Args: []string{act.Service, "status"}}); err != nil {
			a.printf("    %s is not running\n", act.Service)
			return nil
		}
		_, err := a.Runner.Run(ctx, Cmd{Name: "service", Args: []string{act.Service, "stop"}})
		return err
	case ActRemovePath:
		return a.removePath(act.File)
	case ActPwDelete:
		return a.pwDelete(ctx, act.Name)
	}
	return fmt.Errorf("unknown action %q", act.Type)
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
		return Cmd{Name: "pw", Args: args}
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
	if SysrcCanEdit(act.Name) {
		args := []string{"-f", full, act.Name + act.Op + act.Value}
		if act.Op == "-x" {
			args = []string{"-f", full, "-x", act.Name}
		}
		if _, err := a.Runner.Run(ctx, Cmd{Name: "sysrc", Args: args}); err != nil {
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
// and the file is replaced atomically with its mode and owner. The caller
// reads the result back.
func (a *Applier) editTunable(full string, act Action) error {
	if !tunableNameRE.MatchString(act.Name) {
		return fmt.Errorf("%s: %q is neither a shell variable nor a loader tunable name", act.File, act.Name)
	}
	b, err := os.ReadFile(full)
	missing := errors.Is(err, fs.ErrNotExist)
	if err != nil && !missing {
		return err
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
	if st, err := os.Stat(full); err == nil {
		mode = st.Mode().Perm()
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(sys.Uid), int(sys.Gid)
		}
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
	return os.Rename(name, full)
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
	js, err := LoadJournals(root)
	if err != nil {
		return RollbackReport{}, err
	}
	var j *Journal
	for i := len(js) - 1; i >= 0; i-- {
		if (id == "" && js[i].RolledBack == nil) || js[i].ID == id {
			j = &js[i]
			break
		}
	}
	if j == nil {
		if id == "" {
			return RollbackReport{}, errors.New("no run to roll back")
		}
		return RollbackReport{}, fmt.Errorf("no journal %s", id)
	}
	if j.RolledBack != nil {
		return RollbackReport{}, fmt.Errorf("%s was rolled back at %s", j.ID, j.RolledBack.Format(time.RFC3339))
	}
	r := RollbackReport{ID: j.ID}
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
			// In place: the file keeps its inode, owner, and mode.
			if err := os.WriteFile(full, b, 0); err != nil {
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
