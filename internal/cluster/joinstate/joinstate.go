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
	"github.com/greenbeanorg/skep/internal/cluster/layout"
)

// FileName is the file's name in the data directory.
const FileName = layout.JoinFile

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
	// itself (ADR 0013 B1 step 3.7); health reports it. The node accepts
	// no inbound key in this phase (decision D10 of step 8b-2a).
	PhaseRefused = "refused"
	// PhaseAbandoning: "skep-cluster join --abandon" has decided to put
	// the one-node state back and is doing its file steps. The service
	// refuses to start in this phase; running the command again finishes
	// them without asking the members again (decision D9).
	PhaseAbandoning = "abandoning"
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
	// Guests is the host's guest list the join call carries (step 8d-2,
	// D14), the same on every repeat; omitted when the host has none.
	// A build before step 8d-2 cannot read a join.json that holds it.
	Guests []clusterstate.JoinGuest `json:"guests,omitempty"`
}

const format = 1

// maxFile is the most of join.json that Read reads.
const maxFile = 512 << 10

// Peers returns the nodes a joining node accepts and may dial, without
// itself: the join call's answer once one has been accepted (it replaces
// the admission's list, decision D10 of step 8b-2a), the admission's
// members before.
func (s State) Peers(self string) []joincode.Node {
	seen := map[string]bool{}
	var out []joincode.Node
	list := s.Admission.Members
	if len(s.Answer) > 0 {
		list = s.Answer
	}
	for _, n := range list {
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
	case PhaseCalling, PhaseUnknown, PhaseAccepted, PhaseRefused, PhaseAbandoning:
	default:
		return fmt.Errorf("phase %q is unknown", s.Phase)
	}
	if s.Format != format || s.OpID == "" || !clusterstate.ValidNodeID(s.Admission.ClusterID) {
		return errors.New("format, operation ID or admission missing")
	}
	if s.Standalone == "" || filepath.Base(s.Standalone) != s.Standalone {
		return fmt.Errorf("standalone directory %q is not a name in the data directory", s.Standalone)
	}
	if len(s.Guests) > 0 {
		if err := clusterstate.CheckJoinGuestLimits(s.Guests); err != nil {
			return err
		}
		return clusterstate.CheckJoinGuests(s.Guests)
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
	// 512 KiB (step 8d-2, D14): the guest list is at most 160 KiB as
	// JSON, more indented. One byte more is read to tell a file over the
	// limit from a damaged one.
	b, err := io.ReadAll(io.LimitReader(fh, maxFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFile {
		return nil, fmt.Errorf("%s is larger than 512 KiB", FileName)
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
	return layout.SyncDir(dir)
}

// Remove deletes the file, then fsyncs dir.
func Remove(dir string) error {
	return layout.RemoveFile(filepath.Join(dir, FileName))
}

// AllNodes returns every node of the admission and of the stored answer,
// without itself and without duplicates: the nodes "join --abandon" asks.
func (s State) AllNodes(self string) []joincode.Node {
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

// StandalonePrefix starts the name of a moved-aside one-node state.
const StandalonePrefix = layout.StandalonePrefix

// MoveAside moves the one-node state aside: one rename of state/ to a new
// standalone.<UTC time> in dir, then an fsync of dir (decision D1 of step
// 8b-2a). It returns the new name.
func MoveAside(dir, utc string) (string, error) {
	name := StandalonePrefix + utc
	if ok, err := layout.Exists(filepath.Join(dir, name)); err != nil || ok {
		if err == nil {
			err = fmt.Errorf("%s exists already", name)
		}
		return "", err
	}
	if err := os.Rename(layout.State(dir), filepath.Join(dir, name)); err != nil {
		return "", err
	}
	return name, layout.SyncDir(dir)
}

// PutBack puts a moved-aside state back: one rename of standalone to
// state/, then an fsync of dir. It refuses, changing nothing, when state/
// exists.
func PutBack(dir, standalone string) error {
	if !layout.IsStandaloneName(standalone) {
		return fmt.Errorf("%q is not a moved-aside state", standalone)
	}
	if ok, err := layout.Exists(layout.State(dir)); err != nil || ok {
		if err == nil {
			err = fmt.Errorf("%s exists; the one-node state was not put back", layout.State(dir))
		}
		return err
	}
	if err := os.Rename(filepath.Join(dir, standalone), layout.State(dir)); err != nil {
		return err
	}
	return layout.SyncDir(dir)
}

// DeleteStandalone removes a moved-aside state for good.
func DeleteStandalone(dir, standalone string) error {
	if !layout.IsStandaloneName(standalone) {
		return fmt.Errorf("%q is not a moved-aside state", standalone)
	}
	return layout.RemoveAll(filepath.Join(dir, standalone))
}
