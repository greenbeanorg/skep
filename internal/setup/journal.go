package setup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// JournalFormat is the version of journal.json.
const JournalFormat = 1

// journalRoot is where every apply and remove records what it did, under
// the root: /var/db/skep/setup/<UTC timestamp>/ (0700 root:wheel), with
// journal.json and a copy of every file before it was changed (ADR 0012
// Decision 4).
const journalRoot = "var/db/skep/setup"

var journalIDRE = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z$`)

// Journal is one run of apply or remove.
type Journal struct {
	Format      int        `json:"format"`
	ID          string     `json:"id"`
	Command     string     `json:"command"`
	SkepVersion string     `json:"skep_version"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	// Complete is set once every entry has run. A journal that is not
	// complete makes the next apply refuse until it is rolled back.
	Complete   bool       `json:"complete"`
	RolledBack *time.Time `json:"rolled_back_at,omitempty"`
	Entries    []Entry    `json:"entries"`
	// Unreadable is set, by LoadJournals, for a journal directory whose
	// journal.json is missing or not JSON (a run that stopped before its
	// first save, or a damaged file). Such a run counts as unfinished
	// with no recorded changes; rolling it back only marks it so.
	Unreadable string `json:"-"`
}

// Entry is one action of a run, with the state of the file it changed.
type Entry struct {
	Action Action `json:"action"`
	// File is the host path the action changed, if any.
	File string `json:"file,omitempty"`
	// Backup is the copy of File before the change, relative to the
	// journal directory; empty when File did not exist.
	Backup       string `json:"backup,omitempty"`
	BeforeSHA256 string `json:"before_sha256,omitempty"`
	AfterSHA256  string `json:"after_sha256,omitempty"`
	Done         bool   `json:"done"`
	// Skipped is set when the action was not made because the host
	// changed between the plan and the action (the variable was set or
	// changed meanwhile); Done stays false.
	Skipped bool   `json:"skipped,omitempty"`
	Error   string `json:"error,omitempty"`
}

// JournalDir is the directory of one journal under root.
func JournalDir(root, id string) string { return filepath.Join(root, journalRoot, id) }

// LoadJournals returns every journal under root, oldest first.
func LoadJournals(root string) ([]Journal, error) {
	entries, err := os.ReadDir(filepath.Join(root, journalRoot))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Journal
	for _, e := range entries {
		if !e.IsDir() || !journalIDRE.MatchString(e.Name()) {
			continue
		}
		j, err := LoadJournal(root, e.Name())
		var ue *unreadableError
		if errors.As(err, &ue) {
			// Not every command must fail on one damaged journal: it is
			// an unfinished run, which the plans refuse on until
			// "skep-setup rollback ID" clears it.
			j = Journal{Format: JournalFormat, ID: e.Name(), Command: "unknown", Unreadable: ue.Error()}
		} else if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}

// LoadJournal reads one journal.
func LoadJournal(root, id string) (Journal, error) {
	if !journalIDRE.MatchString(id) {
		return Journal{}, fmt.Errorf("journal %q: not a journal name (UTC timestamp such as 20261122T150000Z)", id)
	}
	b, err := os.ReadFile(filepath.Join(JournalDir(root, id), "journal.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return Journal{}, &unreadableError{fmt.Errorf("journal %s: no journal.json", id)}
	}
	if err != nil {
		return Journal{}, fmt.Errorf("journal %s: %w", id, err)
	}
	var j Journal
	if err := json.Unmarshal(b, &j); err != nil {
		var se *json.SyntaxError
		if errors.As(err, &se) || errors.Is(err, io.ErrUnexpectedEOF) || len(b) == 0 {
			return Journal{}, &unreadableError{fmt.Errorf("journal %s: journal.json is not JSON: %w", id, err)}
		}
		return Journal{}, fmt.Errorf("journal %s: %w", id, err)
	}
	if j.Format != JournalFormat || j.ID != id {
		// Set aside like an unreadable one, so that one such journal (a
		// newer skep-setup's, or a copied directory) does not make every
		// command fail: the plans refuse on it as an unfinished run until
		// "skep-setup rollback ID" clears it, which undoes nothing and
		// keeps the file as journal.json.unreadable.
		return Journal{}, &unreadableError{fmt.Errorf("journal %s: format %d id %q not understood (this skep-setup writes format %d; another version, or a copied directory?)",
			id, j.Format, printableID(j.ID), JournalFormat)}
	}
	return j, nil
}

// printableID cuts an ID read from a file to something safe to print.
func printableID(s string) string {
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

// unreadableError is a journal directory without a readable journal.json.
type unreadableError struct{ err error }

func (e *unreadableError) Error() string { return e.err.Error() }
func (e *unreadableError) Unwrap() error { return e.err }

// Describe is one line naming the run, for the operator.
func (j Journal) Describe() string {
	if j.Unreadable != "" {
		return fmt.Sprintf("run %s: unfinished, no recorded changes (%s)", j.ID, j.Unreadable)
	}
	n := 0
	for _, e := range j.Entries {
		if e.Done {
			n++
		}
	}
	state := "finished"
	if !j.Complete {
		state = "unfinished"
	}
	return fmt.Sprintf("run %s: %s, started %s, %s, %d of %d change(s) made",
		j.ID, j.Command, j.StartedAt.UTC().Format(time.RFC3339), state, n, len(j.Entries))
}

// save writes the journal atomically.
func (j *Journal) save(root string) error {
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(JournalDir(root, j.ID), "journal.json"), append(b, '\n'), 0o600)
}

// incompleteJournal returns the newest journal that did not finish and
// was not rolled back, if any.
func incompleteJournal(js []Journal) *Journal {
	for i := len(js) - 1; i >= 0; i-- {
		if !js[i].Complete && js[i].RolledBack == nil {
			return &js[i]
		}
	}
	return nil
}

// writtenBySetup reports whether the content hash of path is one an apply
// that was not rolled back left there.
func writtenBySetup(js []Journal, path, sum string) bool {
	for _, j := range js {
		if j.RolledBack != nil {
			continue
		}
		for _, e := range j.Entries {
			if e.File == path && e.Done && e.AfterSHA256 == sum {
				return true
			}
		}
	}
	return false
}

// fileSHA256 returns the hex SHA-256 of a file, or "" if it does not exist.
func fileSHA256(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return sha256Hex(b), nil
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// writeAtomic writes path through a temporary file in the same directory.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".skep-setup-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
