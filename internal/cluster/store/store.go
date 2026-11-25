// Package store is skep-cluster's Raft store: hashicorp/raft with a bbolt
// log and stable store and the library's file snapshots, all pure Go
// (ADR 0013 A2, B11). It is the only package that imports the Raft
// library.
//
// A node with a listen address runs Raft over the network transport of
// package raftnet (TLS 1.3 on port 8444, per-RPC identity); a single host
// without one uses the library's in-memory transport, which reaches no
// other process and opens no port. With raftnet, this is the only package
// that imports the Raft library.
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
	"sync"
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
	// ConfigChangeWait bounds Skep's wait for a configuration change to
	// commit. The library's own wait has no timeout (v1.8.0
	// requestConfigChange: the timeout covers only the hand-off); it ends
	// with ErrLeadershipLost when the leader steps down.
	ConfigChangeWait = 30 * time.Second
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
	// Dir is the state directory (layout.StateDir in the data
	// directory): raft.db and snapshots/.
	Dir string
	// NodeID is this node's ID, its server ID in the Raft configuration.
	NodeID string
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Transport carries Raft RPCs to other nodes (a *raftnet.Transport).
	// Nil uses the in-memory transport: a single host.
	Transport raft.Transport
	// Empty creates an empty store when the directory holds none: a
	// joining node starts with an empty log and no configuration, and
	// receives both from the leader (ADR 0013 B1, join command step 4).
	Empty bool
	// Machine is the state machine to feed. Nil makes a new one.
	Machine *clusterstate.Machine
	// Quiet discards the library's own log lines. A command that opens a
	// store for a moment (init, the last step of "leave --wipe") sets it,
	// so that the library's start-up warnings do not reach the terminal;
	// errors still come back as errors.
	Quiet bool
}

// SnapshotThreshold is the library's default snapshot threshold, which
// Skep keeps: a non-voter has caught up when its applied index is within
// this many entries of the leader's commit index (ADR 0013 B1, step 3).
var SnapshotThreshold = raft.DefaultConfig().SnapshotThreshold

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

func openBackends(dir string, quiet bool) (*backends, error) {
	bolt, err := raftboltdb.New(raftboltdb.Options{
		Path:        filepath.Join(dir, DBFile),
		BoltOptions: &bbolt.Options{Timeout: openTimeout},
	})
	if err != nil {
		return nil, fmt.Errorf("store: open %s (is skep-cluster already running?): %w", filepath.Join(dir, DBFile), err)
	}
	snaps, err := raft.NewFileSnapshotStoreWithLogger(dir, retainSnapshots, raftLogger(quiet))
	if err != nil {
		bolt.Close()
		return nil, fmt.Errorf("store: snapshots: %w", err)
	}
	return &backends{bolt: bolt, snaps: snaps}, nil
}

// raftLogger sends the library's warnings and errors to stderr as JSON
// lines, next to the service's own log; quiet discards them.
func raftLogger(quiet bool) hclog.Logger {
	if quiet {
		return hclog.NewNullLogger()
	}
	return hclog.New(&hclog.LoggerOptions{Name: "raft", Level: hclog.Warn, Output: os.Stderr, JSONFormat: true})
}

func raftConfig(nodeID string, quiet bool) *raft.Config {
	c := raft.DefaultConfig() // library defaults, pre-vote on (ADR 0013 B11)
	c.LocalID = raft.ServerID(nodeID)
	c.Logger = raftLogger(quiet)
	return c
}

// Bootstrap creates the store in o.Dir with a Raft configuration of this
// node alone. peerAddress is the node's address for other nodes, or "".
// It refuses a directory that already holds a store.
func Bootstrap(o Options, peerAddress string) error {
	if Exists(o.Dir) {
		return fmt.Errorf("store: %s already holds a cluster store", o.Dir)
	}
	if err := makeDir(o.Dir); err != nil {
		return fmt.Errorf("store: create %s: %w", o.Dir, err)
	}
	b, err := openBackends(o.Dir, o.Quiet)
	if err != nil {
		return err
	}
	defer b.bolt.Close()
	addr := raftAddress(peerAddress)
	_, trans := raft.NewInmemTransport(addr)
	defer trans.Close()
	cfg := raft.Configuration{Servers: []raft.Server{{Suffrage: raft.Voter, ID: raft.ServerID(o.NodeID), Address: addr}}}
	if err := raft.BootstrapCluster(raftConfig(o.NodeID, o.Quiet), b.bolt, b.bolt, b.snaps, trans, cfg); err != nil {
		return fmt.Errorf("store: bootstrap: %w", err)
	}
	return nil
}

// Node is the open store with its running Raft node.
type Node struct {
	id      string
	raft    *raft.Raft
	bolt    *raftboltdb.BoltStore
	trans   raft.Transport
	machine *clusterstate.Machine
	log     *slog.Logger

	cacheMu   sync.Mutex
	cache     map[string]bool
	cacheTime time.Time

	closeOnce sync.Once
	closeErr  error
}

// Open opens the store and starts the Raft node.
func Open(o Options) (*Node, error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if !Exists(o.Dir) && !o.Empty {
		return nil, fmt.Errorf("%s: %w", o.Dir, ErrNotInitialised)
	}
	if o.Empty {
		// A joining node's empty state directory (decision D1 of step
		// 8b-2a), created as the service account with an fsync of the
		// data directory.
		if err := makeDir(o.Dir); err != nil {
			return nil, fmt.Errorf("store: create %s: %w", o.Dir, err)
		}
	}
	b, err := openBackends(o.Dir, o.Quiet)
	if err != nil {
		return nil, err
	}
	machine := o.Machine
	if machine == nil {
		machine = clusterstate.New()
	}
	trans := o.Transport
	if trans == nil {
		// The transport's own address does not matter while the node
		// is alone: a single voter elects itself without sending
		// anything.
		_, trans = raft.NewInmemTransport(raft.ServerAddress("local-" + o.NodeID))
	}
	r, err := raft.NewRaft(raftConfig(o.NodeID, o.Quiet), &fsm{machine: machine, log: o.Logger}, b.bolt, b.bolt, b.snaps, trans)
	if err != nil {
		closeTransport(trans)
		b.bolt.Close()
		return nil, fmt.Errorf("store: start raft: %w", err)
	}
	return &Node{id: o.NodeID, raft: r, bolt: b.bolt, trans: trans, machine: machine, log: o.Logger}, nil
}

func closeTransport(t raft.Transport) {
	if c, ok := t.(raft.WithClose); ok {
		_ = c.Close()
	}
}

// Close stops the Raft node and closes the store.
// It may be called more than once; only the first call does anything.
func (n *Node) Close() error {
	n.closeOnce.Do(func() {
		err := n.raft.Shutdown().Error()
		closeTransport(n.trans)
		if cerr := n.bolt.Close(); err == nil {
			err = cerr
		}
		n.closeErr = err
	})
	return n.closeErr
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
type fsm struct {
	machine *clusterstate.Machine
	log     *slog.Logger
}

func (f *fsm) Apply(l *raft.Log) any {
	res := f.machine.Apply(l.Index, l.Data)
	if f.log != nil {
		// Every membership event is one log line on every node, with
		// actor, operation ID and origin (the cluster audit, ADR 0013
		// B10, is the next run).
		for _, ev := range res.Events {
			f.log.Info("cluster membership: "+ev.Kind, "event", ev.Kind, "node_id", ev.NodeID, "name", ev.Name,
				"actor", res.Entry.Actor, "op_id", res.Entry.OpID, "origin", res.Entry.Origin, "index", l.Index)
		}
		if res.Err != nil && errors.Is(res.Err, clusterstate.ErrRefused) {
			f.log.Warn("cluster entry refused by the state machine", "type", res.Entry.Type, "op_id", res.Entry.OpID,
				"actor", res.Entry.Actor, "origin", res.Entry.Origin, "index", l.Index, "error", res.Err.Error())
		}
	}
	return res
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

// raftErr maps a configuration change's error.
func raftErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) || errors.Is(err, raft.ErrLeadershipTransferInProgress) {
		return fmt.Errorf("%w (%v)", ErrNotLeader, err)
	}
	return fmt.Errorf("%w (%v)", ErrOutcomeUnknown, err)
}

// waitFuture waits for a configuration change, at most ConfigChangeWait.
// A change still in flight after that is an unknown outcome: the next
// round reads the configuration again.
func waitFuture(f raft.IndexFuture) error {
	done := make(chan error, 1)
	go func() { done <- f.Error() }()
	select {
	case err := <-done:
		return raftErr(err)
	case <-time.After(ConfigChangeWait):
		return fmt.Errorf("%w (the configuration change did not commit within %s)", ErrOutcomeUnknown, ConfigChangeWait)
	}
}

// RemoveServer removes a server from the Raft configuration (removal step
// 1). For a server that is already absent it changes nothing the library
// would not (v1.8.0 configuration.go nextConfiguration).
func (n *Node) RemoveServer(id string) error {
	err := waitFuture(n.raft.RemoveServer(raft.ServerID(id), 0, applyTimeout))
	n.invalidateCache()
	return err
}

// TransferLeadership hands the leadership to the given voter (decision D6:
// a leader asked to remove itself, or a leader that finds itself
// removing). The caller picks the target: hashicorp/raft v1.8.0 does not
// check a named target's suffrage (raft.go leaderLoop). nil means this
// node is no longer the leader.
func (n *Node) TransferLeadership(id, address string) error {
	if err := n.raft.LeadershipTransferToServer(raft.ServerID(id), raft.ServerAddress(address)).Error(); err != nil {
		return err
	}
	if n.IsLeader() {
		return errors.New("this node is still the leader after the transfer")
	}
	return nil
}

// LeaderCh delivers true when this node becomes the leader and false when
// it stops being one (the library's channel, which keeps only the latest
// value). The service is its only reader.
func (n *Node) LeaderCh() <-chan bool { return n.raft.LeaderCh() }

// Term returns the current Raft term.
func (n *Node) Term() uint64 { return n.raft.CurrentTerm() }

// Configuration returns the latest Raft configuration (committed or not),
// the log index of the latest configuration entry above the commit index
// (0 when there is none), and whether the latest configuration is
// committed: no configuration entry is stored above the commit index.
// The library's GetConfiguration cannot say this in v1.8.0: it returns
// only the latest configuration, from a copy outside the main loop, and
// its Index() is always 0 (api.go GetConfiguration).
func (n *Node) Configuration() ([]Server, uint64, bool, error) {
	f := n.raft.GetConfiguration()
	if err := f.Error(); err != nil {
		return nil, 0, false, fmt.Errorf("store: read the raft configuration: %w", err)
	}
	var out []Server
	for _, s := range f.Configuration().Servers {
		out = append(out, Server{ID: string(s.ID), Address: string(s.Address), Voter: s.Suffrage == raft.Voter})
	}
	idx, committed := n.uncommittedConfiguration()
	return out, idx, committed, nil
}

// uncommittedConfiguration looks at the entries stored above the commit
// index for a configuration entry. It looks at no more than 4096 entries
// and answers "not committed" beyond that.
func (n *Node) uncommittedConfiguration() (uint64, bool) {
	commit, last := n.raft.CommitIndex(), n.raft.LastIndex()
	if last <= commit {
		return 0, true
	}
	if last-commit > 4096 {
		return last, false
	}
	var found uint64
	var l raft.Log
	for i := commit + 1; i <= last; i++ {
		if err := n.bolt.GetLog(i, &l); err != nil {
			continue // compacted by a snapshot: committed
		}
		if l.Type == raft.LogConfiguration || l.Type == raft.LogAddPeerDeprecated || l.Type == raft.LogRemovePeerDeprecated {
			found = i
		}
	}
	return found, found == 0
}

// AddNonvoter adds a server without a vote (join step 2). For a server
// that is already in the configuration it changes nothing the library
// would not (v1.8.0 configuration.go nextConfiguration).
func (n *Node) AddNonvoter(id, address string) error {
	err := waitFuture(n.raft.AddNonvoter(raft.ServerID(id), raft.ServerAddress(address), 0, applyTimeout))
	n.invalidateCache()
	return err
}

// AddVoter gives a server a vote (join step 3); on a server that already
// votes it changes nothing.
func (n *Node) AddVoter(id, address string) error {
	err := waitFuture(n.raft.AddVoter(raft.ServerID(id), raft.ServerAddress(address), 0, applyTimeout))
	n.invalidateCache()
	return err
}

// Leader returns the leader's node ID and address as this node knows them,
// or "" when there is none.
func (n *Node) Leader() (id, address string) {
	addr, lid := n.raft.LeaderWithID()
	return string(lid), string(addr)
}

func (n *Node) invalidateCache() {
	n.cacheMu.Lock()
	n.cacheTime = time.Time{}
	n.cacheMu.Unlock()
}

// IsServer reports whether the node is a server in the latest Raft
// configuration (committed or not; what the library itself uses). With
// cached, an answer at most maxAge old may be used, so that a caller on the
// heartbeat fast path never waits for the Raft main loop.
func (n *Node) IsServer(id string, cached bool, maxAge time.Duration) bool {
	if cached {
		n.cacheMu.Lock()
		if !n.cacheTime.IsZero() && time.Since(n.cacheTime) <= maxAge {
			ok := n.cache[id]
			n.cacheMu.Unlock()
			return ok
		}
		n.cacheMu.Unlock()
	}
	servers, err := n.Servers()
	if err != nil {
		return false
	}
	set := make(map[string]bool, len(servers))
	for _, s := range servers {
		set[s.ID] = true
	}
	n.cacheMu.Lock()
	n.cache, n.cacheTime = set, time.Now()
	n.cacheMu.Unlock()
	return set[id]
}

// LibraryEntriesOnly reports whether every log entry in (from, to] is one
// of the library's own (a no-op, a barrier, a configuration change), so
// that a state machine applied up to from is up to date with a library
// applied up to to. It looks at no more than 4096 entries and answers
// false beyond that.
func (n *Node) LibraryEntriesOnly(from, to uint64) bool {
	if to <= from {
		return true
	}
	if to-from > 4096 {
		return false
	}
	var l raft.Log
	for i := from + 1; i <= to; i++ {
		if err := n.bolt.GetLog(i, &l); err != nil {
			return false
		}
		if l.Type == raft.LogCommand {
			return false
		}
	}
	return true
}

// makeDir creates dir with mode 0700 when it is missing and fsyncs its
// parent.
func makeDir(dir string) error {
	err := os.Mkdir(dir, 0o700)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err == nil {
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	d, err := os.Open(filepath.Dir(dir))
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// ReadState rebuilds the replicated state from the store in dir without
// starting a Raft node: the latest snapshot, then every command entry
// after it, through a new state machine. It is for a node that is alone in
// its Raft configuration, where every stored entry is committed (a single
// voter commits what it has stored); it refuses otherwise. The root-run
// commands use it, through a process running as the service account, with
// the service stopped ("listen -none", "leave --wipe").
func ReadState(dir, nodeID string) (clusterstate.View, []Server, error) {
	servers, err := ReadServers(dir, nodeID)
	if err != nil {
		return clusterstate.View{}, nil, err
	}
	if !Alone(servers, nodeID) {
		return clusterstate.View{}, servers, fmt.Errorf("store: the Raft configuration has %d servers; the state is read offline only on a node that is alone", len(servers))
	}
	b, err := openBackends(dir, true)
	if err != nil {
		return clusterstate.View{}, servers, err
	}
	defer b.bolt.Close()
	m := clusterstate.New()
	var from uint64
	metas, err := b.snaps.List()
	if err != nil {
		return clusterstate.View{}, servers, fmt.Errorf("store: snapshots: %w", err)
	}
	if len(metas) > 0 {
		meta, rc, err := b.snaps.Open(metas[0].ID)
		if err != nil {
			return clusterstate.View{}, servers, fmt.Errorf("store: open snapshot: %w", err)
		}
		err = m.Restore(rc)
		rc.Close()
		if err != nil {
			return clusterstate.View{}, servers, err
		}
		from = meta.Index
	}
	last, err := b.bolt.LastIndex()
	if err != nil {
		return clusterstate.View{}, servers, err
	}
	first, err := b.bolt.FirstIndex()
	if err != nil {
		return clusterstate.View{}, servers, err
	}
	if last > from && first > from+1 {
		return clusterstate.View{}, servers, fmt.Errorf("store: the log starts at %d, after the snapshot at %d", first, from)
	}
	var l raft.Log
	for i := from + 1; i <= last; i++ {
		if err := b.bolt.GetLog(i, &l); err != nil {
			return clusterstate.View{}, servers, fmt.Errorf("store: log entry %d: %w", i, err)
		}
		if l.Type == raft.LogCommand {
			m.Apply(l.Index, l.Data)
		}
	}
	return m.View(), servers, nil
}

// LastIndex returns the index of the last entry stored in the log in dir,
// without starting a Raft node; 0 when dir holds no store. The service
// reads it before it opens the store, when its cluster audit is empty: the
// entries up to it are not audited (D4 of step 8b-2b).
func LastIndex(dir string) (uint64, error) {
	if !Exists(dir) {
		return 0, nil
	}
	b, err := openBackends(dir, true)
	if err != nil {
		return 0, err
	}
	defer b.bolt.Close()
	last, err := b.bolt.LastIndex()
	if err != nil {
		return 0, err
	}
	// A snapshot may cover more than the log holds.
	if metas, err := b.snaps.List(); err == nil && len(metas) > 0 && metas[0].Index > last {
		last = metas[0].Index
	}
	return last, nil
}

// ReadServers reads the Raft configuration from the store in dir without
// starting a Raft node or opening a network port: the root-run commands use
// it, through a process running as the service account, while the service
// is stopped (ADR 0013 B1: "is this node alone" is read from the store).
func ReadServers(dir, nodeID string) ([]Server, error) {
	if !Exists(dir) {
		return nil, fmt.Errorf("%s: %w", dir, ErrNotInitialised)
	}
	b, err := openBackends(dir, true)
	if err != nil {
		return nil, err
	}
	defer b.bolt.Close()
	_, trans := raft.NewInmemTransport(raft.ServerAddress("offline-" + nodeID))
	defer trans.Close()
	conf, err := raft.GetConfiguration(raftConfig(nodeID, true), &fsm{machine: clusterstate.New()}, b.bolt, b.bolt, b.snaps, trans)
	if err != nil {
		return nil, fmt.Errorf("store: read the raft configuration: %w", err)
	}
	var out []Server
	for _, s := range conf.Servers {
		out = append(out, Server{ID: string(s.ID), Address: string(s.Address), Voter: s.Suffrage == raft.Voter})
	}
	return out, nil
}
