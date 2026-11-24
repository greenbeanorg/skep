// Package startcheck is the check the root agent makes of its own binary,
// configuration, and state directories before it trusts them (ADR 0004,
// amendment of 2026-11-23). Each path and every parent up to / must be
// owned by UID 0, not writable by its group or others, and carry no ACL
// beyond the entries that mirror its mode. There is no override.
package startcheck

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// maxLinkHops is how many symbolic links one path may pass through.
const maxLinkHops = 16

// Info is what the check needs from lstat(2).
type Info struct {
	UID     uint32
	Perm    fs.FileMode
	Symlink bool
}

// FS is the host, faked in tests.
type FS interface {
	// Lstat reads a path without following a final symbolic link. A
	// missing path is an error matching fs.ErrNotExist.
	Lstat(path string) (Info, error)
	// Readlink returns a symbolic link's target as stored.
	Readlink(path string) (string, error)
	// ACL returns the output of getfacl -q -n for path.
	ACL(path string) (string, error)
}

// Target is one path to check.
type Target struct {
	Path string
	// What names it in the refusal, for example "the agent's binary".
	What string
	// Optional allows the path, or some of its last components, to be
	// missing (a state directory not created yet).
	Optional bool
}

// Refusal is the reason the agent will not start.
type Refusal struct {
	Path string
	Why  string
	Fix  string
}

func (r *Refusal) Error() string {
	s := "refusing to start: " + r.Path + " " + r.Why
	if r.Fix != "" {
		s += ". Fix: " + r.Fix
	}
	return s
}

// Check returns a *Refusal for the first path that fails, in the order of
// targets, outermost directory first.
func Check(fsys FS, targets []Target) error {
	c := checker{fsys: fsys, passed: map[string]bool{}}
	for _, t := range targets {
		if err := c.target(t); err != nil {
			return err
		}
	}
	return nil
}

type checker struct {
	fsys   FS
	passed map[string]bool
}

func (c *checker) target(t Target) error {
	if slices.Contains(strings.Split(filepath.ToSlash(t.Path), "/"), "..") {
		return &Refusal{Path: t.Path, Why: "contains \"..\", which cannot be checked component by component"}
	}
	abs, err := filepath.Abs(t.Path)
	if err != nil {
		return &Refusal{Path: t.Path, Why: "cannot be made absolute: " + err.Error()}
	}
	return c.walk(abs, t)
}

// step is one path component still to walk. viaLink marks a component that
// came from a symbolic link's target: if it is missing the link dangles,
// which is not the same as a state directory not created yet.
type step struct {
	name    string
	viaLink bool
}

func steps(path string, viaLink bool) []step {
	var out []step
	for _, part := range strings.Split(path, "/") {
		if part != "" && part != "." {
			out = append(out, step{part, viaLink})
		}
	}
	return out
}

// walk resolves abs one component at a time, as the kernel does, and checks
// every real directory and file it passes, so the directory holding each
// link in a chain is checked. A link's own owner and mode do not matter: it
// can only be replaced, which takes write access to its directory.
func (c *checker) walk(abs string, t Target) error {
	root, err := c.fsys.Lstat("/")
	if err != nil {
		return &Refusal{Path: "/", Why: "cannot be read: " + err.Error()}
	}
	if err := c.verify("/", root, t.What); err != nil {
		return err
	}
	cur, pending, hops := "/", steps(abs, false), 0
	seen := map[string]bool{}
	for len(pending) > 0 {
		s := pending[0]
		pending = pending[1:]
		if s.name == ".." { // only from a link's target; cur is already resolved
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, s.name)
		st, err := c.fsys.Lstat(next)
		switch {
		case errors.Is(err, fs.ErrNotExist) && s.viaLink:
			return &Refusal{Path: next, Why: "cannot be resolved: a symbolic link on the way to " + abs + " points to it and it does not exist"}
		case errors.Is(err, fs.ErrNotExist) && t.Optional:
			return nil
		case errors.Is(err, fs.ErrNotExist):
			return &Refusal{Path: next, Why: "does not exist"}
		case err != nil:
			return &Refusal{Path: next, Why: "cannot be read: " + err.Error()}
		}
		if !st.Symlink {
			if err := c.verify(next, st, t.What); err != nil {
				return err
			}
			cur = next
			continue
		}
		key := next
		for _, r := range pending {
			key += "/" + r.name
		}
		if seen[key] {
			return &Refusal{Path: next, Why: "is part of a symbolic link loop on the way to " + abs}
		}
		seen[key] = true
		if hops++; hops > maxLinkHops {
			return &Refusal{Path: next, Why: fmt.Sprintf("is more than %d symbolic links deep on the way to %s", maxLinkHops, abs)}
		}
		to, err := c.fsys.Readlink(next)
		if err != nil {
			return &Refusal{Path: next, Why: "cannot be resolved: " + err.Error()}
		}
		if to == "" {
			return &Refusal{Path: next, Why: "cannot be resolved: the symbolic link is empty"}
		}
		if filepath.IsAbs(to) {
			cur = "/"
		}
		pending = append(steps(to, true), pending...)
	}
	return nil
}

// verify checks one real path once.
func (c *checker) verify(p string, st Info, what string) error {
	if c.passed[p] {
		return nil
	}
	if err := c.one(p, st, what); err != nil {
		return err
	}
	c.passed[p] = true
	return nil
}

func (c *checker) one(p string, st Info, what string) error {
	if st.UID != 0 {
		return &Refusal{Path: p, Why: fmt.Sprintf("is owned by UID %d, not root; that user could change %s, which this root process trusts", st.UID, what),
			Fix: "chown root:wheel " + p}
	}
	if perm := st.Perm.Perm(); perm&0o022 != 0 {
		return &Refusal{Path: p, Why: fmt.Sprintf("is writable by its group or others (mode %04o, owner UID %d); a user other than root could change %s, which this root process trusts", perm, st.UID, what),
			Fix: "chmod go-w " + p}
	}
	out, err := c.fsys.ACL(p)
	if err != nil {
		return &Refusal{Path: p, Why: "could not have its ACL read with /bin/getfacl (" + err.Error() + "); the agent refuses what it cannot check"}
	}
	switch aclClass(out) {
	case aclTrivial:
		return nil
	case aclExtended:
		if modeDenyACL(out) {
			return &Refusal{Path: p, Why: fmt.Sprintf("has NFSv4 deny entries because its mode (%04o) gives a class less than a broader one; the agent accepts only an ACL that mirrors its mode without deny entries, so it cannot rule out a change to %s", st.Perm.Perm(), what),
				Fix: "set a mode that needs no deny entries, such as 0755 or 0700: chmod 0755 " + p + ", or chmod 0700 " + p + " if only root should read it"}
		}
		return &Refusal{Path: p, Why: "has an ACL beyond its mode bits, which can let another user change " + what,
			Fix: "review it with getfacl " + p + ", then remove it with setfacl -b " + p}
	default:
		return &Refusal{Path: p, Why: "has getfacl output the agent does not understand; the agent refuses what it cannot check"}
	}
}

type aclResult int

const (
	aclUnknown aclResult = iota
	aclTrivial
	aclExtended
)

var (
	posixEntryRE = regexp.MustCompile(`^(user|group|other)::([r-])([w-])([x-])$`)
	// A well-formed entry of any kind in each format: what is not one of
	// these is output the check does not understand.
	posixAnyRE  = regexp.MustCompile(`^(user|group|other|mask)(:[^:]*)?:[r-][w-][x-]$`)
	nfs4EntryRE = regexp.MustCompile(`^(owner@|group@|everyone@):([r-][w-][x-][p-][d-][D-][a-][A-][R-][W-][c-][C-][o-][s-]):(-{7}):allow$`)
	nfs4ModeRE  = regexp.MustCompile(`^(owner@|group@|everyone@):([r-][w-][x-][p-][d-][D-][a-][A-][R-][W-][c-][C-][o-][s-]):-{7}:(allow|deny)$`)
	nfs4AnyRE   = regexp.MustCompile(`^(owner@|group@|everyone@|user:[^:]+|group:[^:]+):[rwxpdDaARWcCos-]{14}:[fdinIFS-]{7}:(allow|deny)$`)
)

// nfs4Writes are the permissions, at their positions in getfacl's field,
// that let group@ or everyone@ change a file or directory: write, append,
// delete, delete child, write attributes, write extended attributes, write
// ACL, write owner.
var nfs4Writes = map[int]bool{1: true, 3: true, 4: true, 5: true, 7: true, 9: true, 11: true, 12: true}

// modeDenyACL reports whether NFSv4 output has the shape ZFS gives a mode
// such as 0705: at least one deny entry, only the owner@, group@ and
// everyone@ classes, no inheritance flags, and no write access allowed to
// group@ or everyone@. A named user, an inherited entry, or a write grant
// is an ACL someone set, which a new mode may not remove.
func modeDenyACL(out string) bool {
	deny := false
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		m := nfs4ModeRE.FindStringSubmatch(l)
		if m == nil {
			return false
		}
		if m[3] == "deny" {
			deny = true
			continue
		}
		if m[1] == "owner@" {
			continue
		}
		for i, ch := range m[2] {
			if ch != '-' && nfs4Writes[i] {
				return false
			}
		}
	}
	return deny
}

// aclClass classifies getfacl -q -n output. Trivial is exactly one entry
// for each of the three classes, in either format, with no write access
// for the group or others.
func aclClass(out string) aclResult {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return aclUnknown
	}
	for _, l := range lines {
		if !posixAnyRE.MatchString(l) && !nfs4AnyRE.MatchString(l) {
			return aclUnknown
		}
	}
	if len(lines) != 3 {
		return aclExtended
	}
	seen := map[string]bool{}
	for _, l := range lines {
		if m := posixEntryRE.FindStringSubmatch(l); m != nil {
			if seen[m[1]] || (m[1] != "user" && m[3] == "w") {
				return aclExtended
			}
			seen[m[1]] = true
			continue
		}
		m := nfs4EntryRE.FindStringSubmatch(l)
		if m == nil || seen[m[1]] {
			return aclExtended
		}
		seen[m[1]] = true
		if m[1] != "owner@" {
			for i, ch := range m[2] {
				if ch != '-' && nfs4Writes[i] {
					return aclExtended
				}
			}
		}
	}
	posix := seen["user"] && seen["group"] && seen["other"]
	nfs4 := seen["owner@"] && seen["group@"] && seen["everyone@"]
	if !posix && !nfs4 {
		return aclExtended
	}
	return aclTrivial
}
