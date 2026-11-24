// Package store is skep-cluster's Raft store: hashicorp/raft with a bbolt
// log and stable store and the library's file snapshots, all pure Go
// (ADR 0013 A2, B11). It is the only package that imports the Raft
// library.
//
// Step 8a runs a one-node cluster. Raft RPCs between nodes need the
// inbound handler with per-RPC identity, which is step 8b; until then the
// store uses the library's in-memory transport, which reaches no other
// process.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
)

const (
	// DBFile is the bbolt file with the log and the stable store.
	DBFile = "raft.db"
	// NoAddress stands in the Raft configuration for the address of a
	// node without a listener; the library refuses an empty one.
	NoAddress = "no-listener"
	// retainSnapshots is how many snapshots the file store keeps.
	retainSnapshots = 2
	// applyTimeout bounds how long a write waits to enter the log.
	applyTimeout = 10 * time.Second
	// openTimeout bounds the wait for bbolt's file lock, which another
	// running skep-cluster holds.
	openTimeout = 5 * time.Second
)

// ErrNotInitialised means the directory holds no store.
var ErrNotInitialised = errors.New("the cluster store does not exist: run \"skep-cluster init\" as root first")

// ErrNotLeader means this node is not the leader; nothing was written.
var ErrNotLeader = errors.New("this node is not the cluster leader")

// ErrOutcomeUnknown means a write may or may not have committed (ADR 0013
// B11). The caller repeats it with the same operation ID.
var ErrOutcomeUnknown = errors.New("outcome unknown: the write may have been committed; repeat it with the same operation ID")

// Options says where the store is and who this node is.
type Options struct {
	// Dir is the data directory.
	Dir string
	// NodeID is this node's ID, its server ID in the Raft configuration.
	NodeID string
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Server is one server of the Raft configuration.
type Server struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	Voter   bool   `json:"voter"`
}

// Stats is the state of the local Raft node.
type Stats struct {
	// State is "leader", "follower", "candidate" or "shutdown".
	State        string `json:"state"`
	Term         uint64 `json:"term"`
	LastIndex    uint64 `json:"last_index"`
	CommitIndex  uint64 `json:"commit_index"`
	AppliedIndex uint64 `json:"applied_index"`
	// LeaderID is the leader's node ID, or "" when there is none.
	LeaderID string `json:"leader_id"`
}

// Exists reports whether dir holds a store.
func Exists(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, DBFile))
	return err == nil
}

// raftAddress is the address a node has in the Raft configuration.
func raftAddress(peerAddress string) raft.ServerAddress {
	if peerAddress == "" {
		return NoAddress
	}
	return raft.ServerAddress(peerAddress)
}

type backends struct {
	bolt  *raftboltdb.BoltStore
	snaps *raft.FileSnapshotStore
}

func openBackends(dir string) (*backends, error) {
	bolt, err := raftboltdb.New(raftboltdb.Options{
		Path:        filepath.Join(dir, DBFile),
		BoltOptions: &bbolt.Options{Timeout: openTimeout},
	})
	if err != nil {
		return nil, fmt.Errorf("store: open %s (is skep-cluster already running?): %w", filepath.Join(dir, DBFile), err)
	}
	snaps, err := raft.NewFileSnapshotStoreWithLogger(dir, retainSnapshots, raftLogger())
	if err != nil {
		bolt.Close()
		return nil, fmt.Errorf("store: snapshots: %w", err)
	}
	return &backends{bolt: bolt, snaps: snaps}, nil
}

// raftLogger sends the library's warnings and errors to stderr as JSON
// lines, next to the service's own log.
func raftLogger() hclog.Logger {
	return hclog.New(&hclog.LoggerOptions{Name: "raft", Level: hclog.Warn, Output: os.Stderr, JSONFormat: true})
}

func raftConfig(nodeID string) *raft.Config {
	c := raft.DefaultConfig() // library defaults, pre-vote on (ADR 0013 B11)
	c.LocalID = raft.ServerID(nodeID)
	c.Logger = raftLogger()
	return c
}

// Bootstrap creates the store in o.Dir with a Raft configuration of this
// node alone. peerAddress is the node's address for other nodes, or "".
// It refuses a directory that already holds a store.
func Bootstrap(o Options, peerAddress string) error {
	if Exists(o.Dir) {
		return fmt.Errorf("store: %s already holds a cluster store", o.Dir)
	}
	b, err := openBackends(o.Dir)
	if err != nil {
		return err
	}
	defer b.bolt.Close()
	addr := raftAddress(peerAddress)
	_, trans := raft.NewInmemTransport(addr)
	defer trans.Close()
	cfg := raft.Configuration{Servers: []raft.Server{{Suffrage: raft.Voter, ID: raft.ServerID(o.NodeID), Address: addr}}}
	if err := raft.BootstrapCluster(raftConfig(o.NodeID), b.bolt, b.bolt, b.snaps, trans, cfg); err != nil {
		return fmt.Errorf("store: bootstrap: %w", err)
	}
	return nil
}

// Node is the open store with its running Raft node.
type Node struct {
	id      string
	raft    *raft.Raft
	bolt    *raftboltdb.BoltStore
	trans   *raft.InmemTransport
	machine *clusterstate.Machine
	log     *slog.Logger
}

// Open opens the store and starts the Raft node.
func Open(o Options) (*Node, error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if !Exists(o.Dir) {
		return nil, fmt.Errorf("%s: %w", o.Dir, ErrNotInitialised)
	}
	b, err := openBackends(o.Dir)
	if err != nil {
		return nil, err
	}
	machine := clusterstate.New()
	// The transport's own address does not matter while the node is
	// alone: a single voter elects itself without sending anything.
	_, trans := raft.NewInmemTransport(raft.ServerAddress("local-" + o.NodeID))
	r, err := raft.NewRaft(raftConfig(o.NodeID), &fsm{machine: machine}, b.bolt, b.bolt, b.snaps, trans)
	if err != nil {
		trans.Close()
		b.bolt.Close()
		return nil, fmt.Errorf("store: start raft: %w", err)
	}
	return &Node{id: o.NodeID, raft: r, bolt: b.bolt, trans: trans, machine: machine, log: o.Logger}, nil
}

// Close stops the Raft node and closes the store.
func (n *Node) Close() error {
	err := n.raft.Shutdown().Error()
	n.trans.Close()
	if cerr := n.bolt.Close(); err == nil {
		err = cerr
	}
	return err
}

// Machine returns the replicated state machine, for reads.
func (n *Node) Machine() *clusterstate.Machine { return n.machine }

// IsLeader reports whether this node is the leader.
func (n *Node) IsLeader() bool { return n.raft.State() == raft.Leader }

// WaitLeader waits until this node is the leader.
func (n *Node) WaitLeader(ctx context.Context) error {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		if n.IsLeader() {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("store: this node did not become the leader: %w", ctx.Err())
		case <-t.C:
		}
	}
}

// Apply writes one entry through the log and returns what applying it
// did. ErrNotLeader means nothing was written; any other error but a
// refusal by the state machine is ErrOutcomeUnknown.
func (n *Node) Apply(e clusterstate.Entry) (clusterstate.Result, error) {
	raw, err := e.Encode()
	if err != nil {
		return clusterstate.Result{}, err
	}
	f := n.raft.Apply(raw, applyTimeout)
	if err := f.Error(); err != nil {
		if errors.Is(err, raft.ErrNotLeader) {
			return clusterstate.Result{}, ErrNotLeader
		}
		return clusterstate.Result{}, fmt.Errorf("%w (%v)", ErrOutcomeUnknown, err)
	}
	res, _ := f.Response().(clusterstate.Result)
	return res, res.Err
}

// Barrier waits until every entry written before the call is applied.
func (n *Node) Barrier() error {
	if err := n.raft.Barrier(applyTimeout).Error(); err != nil {
		if errors.Is(err, raft.ErrNotLeader) {
			return ErrNotLeader
		}
		return err
	}
	return nil
}

// Servers returns the latest Raft configuration.
func (n *Node) Servers() ([]Server, error) {
	f := n.raft.GetConfiguration()
	if err := f.Error(); err != nil {
		return nil, fmt.Errorf("store: read the raft configuration: %w", err)
	}
	var out []Server
	for _, s := range f.Configuration().Servers {
		out = append(out, Server{ID: string(s.ID), Address: string(s.Address), Voter: s.Suffrage == raft.Voter})
	}
	return out, nil
}

// Alone reports whether the Raft configuration is this node and no other.
func Alone(servers []Server, nodeID string) bool {
	return len(servers) == 1 && servers[0].ID == nodeID
}

// SetOwnAddress brings a one-node cluster's own records in line with its
// listen address: the member entry (a Skep entry) and the server entry in
// the Raft configuration (ADR 0013 B1, "Where skep-cluster listens").
// peerAddress is "ip:port", or "" for no listener. It refuses unless the
// Raft configuration is this node alone, and changes only what differs,
// so it can be repeated after a crash between the two records.
func (n *Node) SetOwnAddress(peerAddress, opID string) (changed bool, err error) {
	servers, err := n.Servers()
	if err != nil {
		return false, err
	}
	if !Alone(servers, n.id) {
		return false, errors.New("store: the address of a node can be changed this way only while the Raft configuration is this node alone")
	}
	mem, ok := n.machine.Member(n.id)
	if !ok {
		return false, fmt.Errorf("store: node %s has no member entry", n.id)
	}
	if mem.Address != peerAddress {
		e, err := clusterstate.NewEntry(clusterstate.TypeMemberAddress, opID, clusterstate.ActorSystem, n.id,
			clusterstate.MemberAddress{NodeID: n.id, Address: peerAddress})
		if err != nil {
			return false, err
		}
		if _, err := n.Apply(e); err != nil {
			return false, err
		}
		changed = true
	}
	if want := raftAddress(peerAddress); servers[0].Address != string(want) {
		// AddVoter with an existing voter's ID replaces its address
		// (checked in hashicorp/raft v1.8.0, configuration.go).
		if err := n.raft.AddVoter(raft.ServerID(n.id), want, 0, applyTimeout).Error(); err != nil {
			if errors.Is(err, raft.ErrNotLeader) {
				return changed, ErrNotLeader
			}
			return changed, fmt.Errorf("%w (%v)", ErrOutcomeUnknown, err)
		}
		changed = true
	}
	if changed {
		// Return only when both records are applied.
		if err := n.Barrier(); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// Stats returns the state of the Raft node.
func (n *Node) Stats() Stats {
	_, leaderID := n.raft.LeaderWithID()
	return Stats{
		State:        stateName(n.raft.State()),
		Term:         n.raft.CurrentTerm(),
		LastIndex:    n.raft.LastIndex(),
		CommitIndex:  n.raft.CommitIndex(),
		AppliedIndex: n.raft.AppliedIndex(),
		LeaderID:     string(leaderID),
	}
}

func stateName(s raft.RaftState) string {
	switch s {
	case raft.Leader:
		return "leader"
	case raft.Follower:
		return "follower"
	case raft.Candidate:
		return "candidate"
	case raft.Shutdown:
		return "shutdown"
	}
	return "unknown"
}

// Snapshot takes a Raft snapshot now. The tests use it; backup (step 8g)
// will.
func (n *Node) Snapshot() error {
	return n.raft.Snapshot().Error()
}

// fsm adapts the state machine to the library.
type fsm struct{ machine *clusterstate.Machine }

func (f *fsm) Apply(l *raft.Log) any {
	return f.machine.Apply(l.Index, l.Data)
}

func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	b, err := f.machine.Snapshot()
	if err != nil {
		return nil, err
	}
	return fsmSnapshot(b), nil
}

func (f *fsm) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	return f.machine.Restore(rc)
}

type fsmSnapshot []byte

func (s fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(s); err != nil {
		_ = sink.Cancel()
		return err
	}
	return sink.Close()
}

func (fsmSnapshot) Release() {}

// RootOwned returns the paths under dir that belong to root. The root-run
// commands check it after they have worked in the directory: it must be
// empty (ADR 0013 B4, B6: no root-owned file in the store's directory).
func RootOwned(dir string, ownerUID func(fs.FileInfo) (uint32, bool)) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if uid, ok := ownerUID(info); ok && uid == 0 {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}
