// Package layout names what is where in skep-cluster's data directory,
// /var/db/skep/cluster (ADR 0013 B1, amendment 2, decision D1 of step
// 8b-2a), and holds the helpers every step that changes it uses.
//
// The directory holds:
//
//	node.key, node.crt       the node key and its certificate
//	cluster.json             not here: /usr/local/etc/skep (root-owned)
//	state/                   the node's cluster state, 0700:
//	  raft.db                  the Raft log and stable store (bbolt)
//	  snapshots/               the library's file snapshots
//	  membership.json          the applied-membership file (amendment 1)
//	  removed.json             written when the node learns it was removed
//	join.json                while joining (0600)
//	standalone.<UTC time>/   a one-node state moved aside by a join (0700)
//	wipe.json                while "leave --wipe" runs (0600)
//
// Moving a one-node state aside is one rename of state/ to
// standalone.<UTC time>, and putting it back is one rename. Every rename,
// create and delete that a later start depends on is followed by an fsync
// of the parent directory.
package layout

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Names in the data directory.
const (
	// StateDir holds the Raft store, the snapshots, the
	// applied-membership file and the removal record.
	StateDir = "state"
	// StandalonePrefix starts the name of a moved-aside one-node state.
	StandalonePrefix = "standalone."
	// JoinFile is a joining node's record.
	JoinFile = "join.json"
	// WipeMarker is written first and removed last by "skep-cluster
	// leave --wipe". While it exists the service refuses to start.
	WipeMarker = "wipe.json"
	// RemovedFile, inside StateDir, records that this node was removed
	// from its cluster.
	RemovedFile = "removed.json"
	// DirMode is the mode of StateDir and of a standalone directory.
	DirMode fs.FileMode = 0o700
)

// oldLayout is what an 8a or 8b-1 build kept directly in the data
// directory; this build keeps it in StateDir.
var oldLayout = []string{"raft.db", "snapshots", "membership.json"}

// State returns the path of StateDir.
func State(dataDir string) string { return filepath.Join(dataDir, StateDir) }

// ErrOldLayout is returned by CheckOldLayout.
var ErrOldLayout = errors.New("the data directory has the layout of an earlier build")

// CheckOldLayout refuses a data directory that holds raft.db directly, as
// builds before 8b-2a left it. There is no migration code (decision D1):
// the message names the paths to move.
func CheckOldLayout(dataDir string) error {
	if _, err := os.Lstat(filepath.Join(dataDir, "raft.db")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var paths []string
	for _, n := range oldLayout {
		paths = append(paths, filepath.Join(dataDir, n))
	}
	return fmt.Errorf("%w: %s is directly in %s. As the skep-cluster account, with skep_cluster stopped, "+
		"create %s (mode 0700) and move %s into it, then start the service again",
		ErrOldLayout, "raft.db", dataDir, State(dataDir), strings.Join(paths, ", "))
}

// Exists reports whether path exists (without following a last link).
func Exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// SyncDir fsyncs a directory, so that a rename, create or delete in it is
// durable.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// Standalones returns the names of the moved-aside one-node states in
// dataDir, sorted.
func Standalones(dataDir string) ([]string, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && IsStandaloneName(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// IsStandaloneName reports whether name is a moved-aside state's name: a
// name in the data directory (no separator) that starts with
// StandalonePrefix and has something after it.
func IsStandaloneName(name string) bool {
	return filepath.Base(name) == name && len(name) > len(StandalonePrefix) && strings.HasPrefix(name, StandalonePrefix)
}

// MakeState creates StateDir with DirMode when it is missing, and fsyncs
// the data directory. It is run as the service account.
func MakeState(dataDir string) error {
	err := os.Mkdir(State(dataDir), DirMode)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err == nil {
		if err := os.Chmod(State(dataDir), DirMode); err != nil {
			return err
		}
	}
	return SyncDir(dataDir)
}

// RemoveAll deletes path and everything in it, then fsyncs its parent. A
// missing path is not an error.
func RemoveAll(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

// RemoveFile deletes one file, then fsyncs its parent. A missing file is
// not an error.
func RemoveFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return SyncDir(filepath.Dir(path))
}
