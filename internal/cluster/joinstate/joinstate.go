// Package joinstate is a joining node's record of its join,
// /var/db/skep/cluster/join.json. The part of "skep-cluster join" that runs
// as the skep-cluster account writes it after it has moved the one-node
// state aside; the service reads and updates it, and deletes it when the
// node has become a member. Root never writes it.
package joinstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joincode"
)

// FileName is the file's name in the data directory.
const FileName = "join.json"

// Phases of a join.
const (
	// PhaseCalling: the one-node state is moved aside and the join call
	// may have been sent. Read at start, it is treated as unknown.
	PhaseCalling = "calling"
	// PhaseUnknown: the join call was sent and no answer came. The
	// service repeats it with the same operation ID.
	PhaseUnknown = "unknown"
	// PhaseAccepted: the leader answered that join step 1 is applied.
	PhaseAccepted = "accepted"
	// PhaseRefused: after an unknown outcome, a leader refused the
	// repeated call. The service does not put the one-node state back by
	// itself (ADR 0013 B1 step 3.7); health reports it.
	PhaseRefused = "refused"
)

// State is the file's content.
type State struct {
	Format int    `json:"format"`
	Phase  string `json:"phase"`
	// OpID is the join call's operation ID, the same on every repeat.
	OpID string `json:"op_id"`
	// Admission is the admission the join started from.
	Admission joincode.Admission `json:"admission"`
	// Answer is the member entries of the join call's answer, accepted
	// with the admission's (ADR 0013 amendment 1).
	Answer []joincode.Node `json:"answer,omitempty"`
	// Standalone is the directory, inside the data directory, that holds
	// the one-node state moved aside.
	Standalone string `json:"standalone"`
	// Message is the last refusal, for health.
	Message string `json:"message,omitempty"`
}

const format = 1

// Peers returns the nodes a joining node accepts and may dial: the
// admission's members and the answer's, without itself.
func (s State) Peers(self string) []joincode.Node {
	seen := map[string]bool{}
	var out []joincode.Node
	for _, n := range append(append([]joincode.Node(nil), s.Admission.Members...), s.Answer...) {
		if n.NodeID == self || seen[n.NodeID] {
			continue
		}
		seen[n.NodeID] = true
		out = append(out, n)
	}
	return out
}

func (s State) validate() error {
	switch s.Phase {
	case PhaseCalling, PhaseUnknown, PhaseAccepted, PhaseRefused:
	default:
		return fmt.Errorf("phase %q is unknown", s.Phase)
	}
	if s.Format != format || s.OpID == "" || !clusterstate.ValidNodeID(s.Admission.ClusterID) {
		return errors.New("format, operation ID or admission missing")
	}
	if s.Standalone == "" || filepath.Base(s.Standalone) != s.Standalone {
		return fmt.Errorf("standalone directory %q is not a name in the data directory", s.Standalone)
	}
	return nil
}

// Read reads the file in dir; (nil, nil) when there is none.
func Read(dir string) (*State, error) {
	fh, err := os.Open(filepath.Join(dir, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	b, err := io.ReadAll(io.LimitReader(fh, 256<<10))
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var s State
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	return &s, nil
}

// Write replaces the file atomically, mode 0600.
func Write(dir string, s State) error {
	s.Format = format
	if err := s.validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, FileName)
	tmp := path + ".tmp-" + strconv.Itoa(os.Getpid())
	_ = os.Remove(tmp)
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := fh.Write(append(b, '\n')); err != nil {
		fh.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := fh.Sync(); err != nil {
		fh.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := fh.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

// Remove deletes the file.
func Remove(dir string) error {
	if err := os.Remove(filepath.Join(dir, FileName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
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

// StandalonePrefix starts the name of a moved-aside one-node state.
const StandalonePrefix = "standalone."

// MoveAside moves the named entries of dir (the one-node cluster's store,
// snapshots and applied-membership file) into a new directory
// standalone.<UTC time>/ inside dir, mode 0700, and returns its name.
// Entries that do not exist are skipped.
func MoveAside(dir string, names []string, utc string) (string, error) {
	name := StandalonePrefix + utc
	aside := filepath.Join(dir, name)
	if err := os.Mkdir(aside, 0o700); err != nil {
		return "", err
	}
	for _, n := range names {
		if filepath.Base(n) != n {
			return name, fmt.Errorf("%q is not a name", n)
		}
		err := os.Rename(filepath.Join(dir, n), filepath.Join(aside, n))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return name, err
		}
	}
	if err := syncDir(aside); err != nil {
		return name, err
	}
	return name, syncDir(dir)
}

// PutBack moves the named entries from the moved-aside directory back into
// dir and removes that directory. It refuses, changing nothing, when one of
// them exists in dir already.
func PutBack(dir, standalone string, names []string) error {
	if filepath.Base(standalone) != standalone || len(standalone) <= len(StandalonePrefix) || standalone[:len(StandalonePrefix)] != StandalonePrefix {
		return fmt.Errorf("%q is not a moved-aside state", standalone)
	}
	aside := filepath.Join(dir, standalone)
	for _, n := range names {
		if _, err := os.Lstat(filepath.Join(dir, n)); err == nil {
			return fmt.Errorf("%s exists in %s; the one-node state was not put back", n, dir)
		}
	}
	for _, n := range names {
		err := os.Rename(filepath.Join(aside, n), filepath.Join(dir, n))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(aside); err != nil {
		return err
	}
	return syncDir(dir)
}

// DeleteStandalone removes a moved-aside state for good.
func DeleteStandalone(dir, standalone string) error {
	if filepath.Base(standalone) != standalone || len(standalone) <= len(StandalonePrefix) || standalone[:len(StandalonePrefix)] != StandalonePrefix {
		return fmt.Errorf("%q is not a moved-aside state", standalone)
	}
	if err := os.RemoveAll(filepath.Join(dir, standalone)); err != nil {
		return err
	}
	return syncDir(dir)
}
