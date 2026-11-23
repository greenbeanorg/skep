package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Incomplete is an apply or remove that failed after changing something
// (exit 4). It says what was changed, what was not, and what to do next.
type Incomplete struct {
	Command string
	// Problem is what failed.
	Problem string
	// Changed lists the actions that changed the host, Uncertain the
	// failed action when it may have, NotChanged the rest.
	Changed, Uncertain, NotChanged []string
	// Next says whether running the command again is safe.
	Next string
	Err  error
}

func (e *Incomplete) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "incomplete: skep-setup %s: %s.", e.Command, e.Problem)
	writeList(&b, "Changed", e.Changed)
	writeList(&b, "May have changed", e.Uncertain)
	writeList(&b, "Not changed", e.NotChanged)
	b.WriteString(" " + e.Next)
	return b.String()
}

func (e *Incomplete) Unwrap() error { return e.Err }

func writeList(b *strings.Builder, label string, items []string) {
	if len(items) > 0 {
		fmt.Fprintf(b, " %s: %s.", label, strings.Join(items, "; "))
	}
}

// effect is what one action did to the host.
type effect int

const (
	effectNone effect = iota
	effectChanged
	effectUncertain
)

func anyEffect(effects []effect) bool {
	for _, x := range effects {
		if x != effectNone {
			return true
		}
	}
	return false
}

// newIncomplete sorts the run's entries by what they did. An unfinished
// run makes the next plan refuse, so the advice is always to roll back.
func newIncomplete(j *Journal, effects []effect, problem string) *Incomplete {
	inc := &Incomplete{Command: j.Command, Problem: problem}
	inc.sort(j, effects)
	if j.Command == "remove" {
		inc.Next = "Do not run remove again yet: it refuses until this run is cleared with skep-setup rollback " + j.ID +
			", which puts back the rc.conf and loader.conf lines it changed but does not start services, restore deleted data, or create the account again"
	} else {
		inc.Next = "Do not run apply again yet: it refuses until this run is undone with skep-setup rollback " + j.ID +
			", which puts back the files it changed but does not stop services or delete users"
	}
	return inc
}

func (inc *Incomplete) sort(j *Journal, effects []effect) {
	for i, e := range j.Entries {
		name := e.Action.String()
		switch {
		case effects[i] == effectChanged:
			inc.Changed = append(inc.Changed, name)
		case effects[i] == effectUncertain:
			inc.Uncertain = append(inc.Uncertain, name)
		case e.Skipped:
			inc.NotChanged = append(inc.NotChanged, name+" (skipped)")
		case e.Done:
			inc.NotChanged = append(inc.NotChanged, name+" (already so)")
		default:
			inc.NotChanged = append(inc.NotChanged, name)
		}
	}
}

// adminIncomplete is the run in which only the administrator failed: the
// journal is complete, so running the same command again is safe. It is nil
// when the run changed nothing else.
func adminIncomplete(j *Journal, effects []effect, adminErr *AdminCreateError) *Incomplete {
	if !anyEffect(effects) {
		return nil
	}
	inc := &Incomplete{Command: j.Command, Problem: adminErr.Error(), Err: adminErr,
		Next: "Running the same command again is safe: it plans only the administrator"}
	inc.sort(j, effects)
	return inc
}

// RollbackIncomplete is a rollback that failed after restoring or removing
// some files (exit 4). Running it again is safe: a file already put back
// is left as it is.
type RollbackIncomplete struct {
	ID       string
	Restored []string
	Removed  []string
	Err      error
}

func (e *RollbackIncomplete) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "incomplete: skep-setup rollback %s: %v.", e.ID, e.Err)
	writeList(&b, "Restored", e.Restored)
	writeList(&b, "Removed", e.Removed)
	fmt.Fprintf(&b, " Not changed: any other file of the run, and the run is not marked rolled back. Running skep-setup rollback %s again is safe: files already put back are left as they are, though it can fail the same way until the cause is fixed.", e.ID)
	return b.String()
}

func (e *RollbackIncomplete) Unwrap() error { return e.Err }

// RollbackError returns err as a *RollbackIncomplete when the report shows
// the rollback changed files before failing, and unchanged otherwise.
func RollbackError(r RollbackReport, err error) error {
	if err == nil || len(r.Restored)+len(r.Removed) == 0 {
		return err
	}
	return &RollbackIncomplete{ID: r.ID, Restored: r.Restored, Removed: r.Removed, Err: err}
}

// pathState is what an action's path looked like before it ran.
type pathState struct {
	sum           string
	sumErr        error
	exists        bool
	parentMissing bool
}

// snapshot records the path an action may change.
func (a *Applier) snapshot(act Action, file string) pathState {
	var st pathState
	switch act.Type {
	case ActSysrc, ActWrite, ActMkdir, ActRemovePath:
	default:
		return st
	}
	if act.File == "" {
		return st
	}
	full := hostPath(a.Root, act.File)
	_, err := os.Lstat(full)
	st.exists = err == nil
	_, err = os.Lstat(filepath.Dir(full))
	st.parentMissing = errors.Is(err, fs.ErrNotExist)
	if file != "" {
		st.sum, st.sumErr = fileSHA256(full)
	}
	return st
}

// changed reports whether a successful action changed the host. Only a
// tracked file can show that it did not: its content is the same.
func (a *Applier) changed(act Action, file string, before pathState) bool {
	if file == "" || before.sumErr != nil || act.Type == ActRemovePath {
		return true
	}
	now, err := fileSHA256(hostPath(a.Root, file))
	return err != nil || now != before.sum
}

// failedEffect decides what a failed action left. It answers effectNone
// only on evidence: the file is byte-identical, or the path was there before
// (writeNew and mkdir cannot change an existing path: link(2) and mkdir(2)
// fail on it, and neither sets a mode or owner after that) or is still
// absent, with no parent directory missing. A failed service stop is always
// uncertain: the signal may have been delivered even if the service still
// runs.
func (a *Applier) failedEffect(act Action, file string, before pathState) effect {
	switch act.Type {
	case ActSysrc:
		if file != "" && before.sumErr == nil {
			if now, err := fileSHA256(hostPath(a.Root, file)); err == nil && now == before.sum {
				return effectNone
			}
		}
	case ActWrite, ActMkdir:
		if before.parentMissing {
			return effectUncertain
		}
		_, err := os.Lstat(hostPath(a.Root, act.File))
		if before.exists || errors.Is(err, fs.ErrNotExist) {
			return effectNone
		}
	}
	return effectUncertain
}

// interruptedError is a command that was killed or timed out rather than
// exiting by itself, so what it had done is unknown.
type interruptedError struct{ err error }

func (e *interruptedError) Error() string { return e.err.Error() }
func (e *interruptedError) Unwrap() error { return e.err }

// markInterrupted wraps err from a command that ran under ctx when the
// command did not exit by itself.
func markInterrupted(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &ee) && !ee.Exited()) {
		return &interruptedError{err}
	}
	return err
}
