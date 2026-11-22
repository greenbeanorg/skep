// Package dbbackup takes consistent copies of Skep's SQLite databases for
// "skep-agent db backup" and "skep-api db backup" (ADR 0012 Decision 5,
// answer 10). skep-setup upgrade runs both before it restarts the
// services, into one directory per upgrade, and the last three backups are
// kept.
//
// The copy is made with SQLite's VACUUM INTO from a read-only connection,
// so it is consistent while the service runs and the source is never
// written. The audit archive is listed (names, sizes, SHA-256), not
// copied: its segments are already sealed files.
package dbbackup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	// The pure-Go SQLite driver, as everywhere in Skep.
	_ "modernc.org/sqlite"
)

// DefaultKeep is how many backups are kept (answer 10).
const DefaultKeep = 3

// marker is in every backup directory; only such directories are pruned.
const marker = ".skep-backup"

// dirRE is a backup directory's name: <label>-<UTC timestamp>.
var dirRE = regexp.MustCompile(`^[A-Za-z0-9._+]+(-[A-Za-z0-9._+]+)*-([0-9]{8}T[0-9]{6}Z)$`)

// labelRE limits the label to characters safe in a file name.
var labelRE = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)

// Options says what to copy and where.
type Options struct {
	// Name is "agent" or "api": the copy is <Name>.db.
	Name string
	// DBPath is the live database.
	DBPath string
	// ArchiveDir is the audit archive to list; empty or missing lists
	// nothing.
	ArchiveDir string
	// Parent holds the backup directories (<data dir>/backup).
	Parent string
	// Dir is the backup directory. Empty means Parent/<Label>-<timestamp>.
	// Given, it must be directly inside Parent, so pruning sees it.
	Dir   string
	Label string
	// Keep is how many backup directories to keep, this one included.
	Keep int
	Now  func() time.Time
}

// Result is what Backup wrote.
type Result struct {
	Dir     string
	File    string
	SHA256  string
	Archive string   // the archive listing, if any
	Pruned  []string // backup directories removed
}

// Backup copies the database and prunes old backups.
func Backup(ctx context.Context, o Options) (Result, error) {
	if o.Name != "agent" && o.Name != "api" {
		return Result{}, fmt.Errorf("backup name %q: want agent or api", o.Name)
	}
	if o.Keep < 1 {
		return Result{}, errors.New("keep must be at least 1")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if st, err := os.Stat(o.DBPath); err != nil {
		return Result{}, fmt.Errorf("database %s: %w", o.DBPath, err)
	} else if !st.Mode().IsRegular() {
		return Result{}, fmt.Errorf("database %s is not a file", o.DBPath)
	}
	dir := o.Dir
	if dir == "" {
		if !labelRE.MatchString(o.Label) {
			return Result{}, fmt.Errorf("label %q: use letters, digits, . _ + -", o.Label)
		}
		dir = filepath.Join(o.Parent, o.Label+"-"+o.Now().UTC().Format("20060102T150405Z"))
	}
	if filepath.Dir(filepath.Clean(dir)) != filepath.Clean(o.Parent) || !dirRE.MatchString(filepath.Base(dir)) {
		return Result{}, fmt.Errorf("backup directory %s: must be %s/<label>-<UTC timestamp>", dir, o.Parent)
	}
	if err := os.MkdirAll(o.Parent, 0o700); err != nil {
		return Result{}, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return Result{}, err
	}
	r := Result{Dir: dir, File: filepath.Join(dir, o.Name+".db")}
	if _, err := os.Lstat(r.File); err == nil {
		return Result{}, fmt.Errorf("%s exists; a backup is never overwritten", r.File)
	}
	if err := vacuumInto(ctx, o.DBPath, r.File); err != nil {
		os.Remove(r.File)
		return Result{}, err
	}
	if err := os.Chmod(r.File, 0o600); err != nil {
		return Result{}, err
	}
	if err := integrityCheck(ctx, r.File); err != nil {
		return Result{}, fmt.Errorf("the copy %s fails its integrity check: %w", r.File, err)
	}
	sum, err := fileSum(r.File)
	if err != nil {
		return Result{}, err
	}
	r.SHA256 = sum
	if err := os.WriteFile(r.File+".sha256", []byte(sum+"  "+o.Name+".db\n"), 0o600); err != nil {
		return Result{}, err
	}
	if o.ArchiveDir != "" {
		listing, err := listArchive(o.ArchiveDir)
		if err != nil {
			return Result{}, err
		}
		if listing != "" {
			r.Archive = filepath.Join(dir, o.Name+"-audit-archive.txt")
			if err := os.WriteFile(r.Archive, []byte(listing), 0o600); err != nil {
				return Result{}, err
			}
		}
	}
	// The marker last: a directory with it holds a finished backup, and
	// only such directories are counted and pruned. One that failed half
	// way is left for the operator, never pruned as if it were complete.
	if err := os.WriteFile(filepath.Join(dir, marker), nil, 0o600); err != nil {
		return Result{}, err
	}
	r.Pruned, err = prune(o.Parent, o.Keep)
	return r, err
}

func readOnlyDSN(path string) string {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + q.Encode()
}

// vacuumInto writes a consistent copy of src to dst.
func vacuumInto(ctx context.Context, src, dst string) error {
	db, err := sql.Open("sqlite", readOnlyDSN(src))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	return nil
}

func integrityCheck(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		return err
	}
	defer db.Close()
	var res string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return errors.New(res)
	}
	return nil
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return readerSum(f)
}

func readerSum(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// archiveFileSum opens an archive file without following a symbolic link
// (O_NOFOLLOW), requires a regular file, and returns its size and SHA-256
// from that one open file.
func archiveFileSum(path string) (int64, string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, "", err
	}
	if !st.Mode().IsRegular() {
		return 0, "", fmt.Errorf("%s is not a regular file", path)
	}
	sum, err := readerSum(f)
	return st.Size(), sum, err
}

// listArchive returns "<sha256>  <size>  <name>" for every file in the
// archive directory, or "" when there is none.
func listArchive(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		size, sum, err := archiveFileSum(filepath.Join(dir, e.Name()))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s  %d  %s\n", sum, size, e.Name())
	}
	if b.Len() == 0 {
		return "", nil
	}
	return "# Audit archive " + dir + " (listed, not copied)\n" + b.String(), nil
}

// prune removes the oldest backup directories beyond keep. Only
// directories with the marker and a backup name are counted or removed.
func prune(parent string, keep int) ([]string, error) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil, err
	}
	type bdir struct{ name, stamp string }
	var dirs []bdir
	for _, e := range entries {
		m := dirRE.FindStringSubmatch(e.Name())
		if !e.IsDir() || m == nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(parent, e.Name(), marker)); err != nil {
			continue
		}
		dirs = append(dirs, bdir{e.Name(), m[2]})
	}
	sort.Slice(dirs, func(a, b int) bool {
		if dirs[a].stamp != dirs[b].stamp {
			return dirs[a].stamp < dirs[b].stamp
		}
		return dirs[a].name < dirs[b].name
	})
	var pruned []string
	for len(dirs) > keep {
		p := filepath.Join(parent, dirs[0].name)
		if err := os.RemoveAll(p); err != nil {
			return pruned, err
		}
		pruned = append(pruned, p)
		dirs = dirs[1:]
	}
	return pruned, nil
}
