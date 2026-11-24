// Package service is the running skep-cluster: it ties the node key, the
// store, the node-to-node listener and transport, the join and the local
// API's answers together (ADR 0013).
package service

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joinstate"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/membership"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/peerlistener"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
	"github.com/greenbeanorg/skep/internal/cluster/store"
	"github.com/greenbeanorg/skep/internal/ids"
	"github.com/greenbeanorg/skep/pkg/models"
	"github.com/greenbeanorg/skep/pkg/version"
)

// Store is what the service needs from the cluster store.
type Store interface {
	Machine() *clusterstate.Machine
	Stats() store.Stats
	Servers() ([]store.Server, error)
	IsLeader() bool
	Leader() (id, address string)
	SetOwnAddress(peerAddress, opID string) (bool, error)
	Apply(e clusterstate.Entry) (clusterstate.Result, error)
	AddNonvoter(id, address string) error
	AddVoter(id, address string) error
	IsServer(id string, cached bool, maxAge time.Duration) bool
	LibraryEntriesOnly(from, to uint64) bool
	Barrier() error
}

// Timings of the background work.
const (
	// LeaderInterval is how often the leader finishes joins and proposes
	// due expiries, besides when it gains leadership (ADR 0013 B1).
	LeaderInterval = 10 * time.Second
	// PendingLifetime is how long a pending entry is valid.
	PendingLifetime = 30 * time.Minute
	// statusTimeout bounds one status RPC of the promotion check.
	statusTimeout = 5 * time.Second
)

// Options configures the service.
type Options struct {
	Config clusterconf.Config
	Key    *nodekey.Key
	// DataDir is the data directory (join.json, standalone state).
	DataDir string
	// Machine is the state machine the store feeds; Writer keeps its
	// applied-membership file.
	Machine *clusterstate.Machine
	Writer  *membership.Writer
	// Join is the joining node's record, or nil.
	Join *joinstate.State
	// Now defaults to time.Now.
	Now func() time.Time
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Interval is how often the background work runs; default 1 s.
	Interval time.Duration
	// LeaderInterval defaults to LeaderInterval.
	LeaderInterval time.Duration
}

// Service implements localapi.Backend, raftnet.Trust and
// raftnet.PeerHandler.
type Service struct {
	cfg       clusterconf.Config
	key       *nodekey.Key
	dataDir   string
	machine   *clusterstate.Machine
	writer    *membership.Writer
	now       func() time.Time
	log       *slog.Logger
	every     time.Duration
	leaderInt time.Duration
	started   time.Time
	caller    raftnet.Caller

	mu         sync.Mutex
	store      Store
	transport  *raftnet.Transport
	join       *joinstate.State
	listener   *peerlistener.Listener
	listenErr  error
	addrErr    error
	renewErr   error
	joinErr    error
	lastRenew  time.Time
	wasLeader  bool
	nextLeader time.Time
	nextWaits  time.Time
	nextJoin   time.Time
	// waits names, per joining member, the members its promotion waits
	// for, as this node last saw it.
	waits map[string][]string
}

// New returns the service. SetStore and Run complete it.
func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Interval <= 0 {
		o.Interval = time.Second
	}
	if o.LeaderInterval <= 0 {
		o.LeaderInterval = LeaderInterval
	}
	if o.Machine == nil {
		o.Machine = clusterstate.New()
	}
	if o.Writer == nil {
		o.Writer = &membership.Writer{}
	}
	s := &Service{cfg: o.Config, key: o.Key, dataDir: o.DataDir, machine: o.Machine, writer: o.Writer,
		now: o.Now, log: o.Logger, every: o.Interval, leaderInt: o.LeaderInterval, started: o.Now(),
		join: o.Join, waits: map[string][]string{}}
	s.caller = raftnet.Caller{NodeID: o.Config.NodeID, Certificate: s.certificate}
	return s
}

func (s *Service) certificate() *tls.Certificate {
	if s.key == nil {
		return nil
	}
	return s.key.Certificate()
}

// SetStore gives the service its open store, and the network transport
// when the node has a listen address.
func (s *Service) SetStore(st Store, t *raftnet.Transport) {
	s.mu.Lock()
	s.store, s.transport = st, t
	if m := st.Machine(); m != nil {
		s.machine = m
	}
	s.mu.Unlock()
}

func (s *Service) getStore() Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store
}

func (s *Service) joinState() *joinstate.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.join == nil {
		return nil
	}
	c := *s.join
	return &c
}

// Run does the background work until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(s.every)
	defer t.Stop()
	for {
		s.step(ctx)
		select {
		case <-ctx.Done():
			s.mu.Lock()
			l := s.listener
			s.listener = nil
			s.mu.Unlock()
			if l != nil {
				_ = l.Close()
			}
			return
		case <-t.C:
		}
	}
}

func (s *Service) step(ctx context.Context) {
	if s.getStore() == nil {
		return
	}
	if s.joinState() == nil {
		s.reconcileAddress()
	}
	s.ensureListener()
	s.revalidate()
	s.joinStep(ctx)
	s.waitsStep(ctx)
	s.leaderStep(ctx)
	s.renew()
}

// revalidate closes the connections of keys that are no longer accepted.
// The state machine calls it after every entry that changes the membership
// or the pending entries; the background work calls it every second for
// pending entries that expired by this node's clock.
func (s *Service) revalidate() {
	s.mu.Lock()
	l := s.listener
	s.mu.Unlock()
	if l != nil {
		l.Revalidate()
	}
}

// OnApplied is installed as the state machine's hook.
func (s *Service) OnApplied() { s.revalidate() }

// reconcileAddress applies a listen address written by "skep-cluster
// listen" or "join-request" to the membership entry and the Raft
// configuration of a one-node cluster.
func (s *Service) reconcileAddress() {
	st := s.getStore()
	if !st.IsLeader() {
		return
	}
	want := s.cfg.PeerAddress()
	mem, ok := s.machine.Member(s.cfg.NodeID)
	servers, err := st.Servers()
	if err == nil && !store.Alone(servers, s.cfg.NodeID) {
		// A clustered node's address is not changed this way.
		if ok && mem.Address != want {
			err = fmt.Errorf("cluster.json says %q but the member entry says %q; the address of a clustered node cannot be changed in this build", want, mem.Address)
		}
		s.setAddrErr(err)
		return
	}
	var raftAddr string
	if err == nil {
		raftAddr = servers[0].Address
	}
	wantRaft := want
	if wantRaft == "" {
		wantRaft = store.NoAddress
	}
	if err == nil && ok && mem.Address == want && raftAddr == wantRaft {
		s.setAddrErr(nil)
		return
	}
	if err == nil && !ok {
		err = fmt.Errorf("node %s has no member entry in the replicated state", s.cfg.NodeID)
	}
	if err == nil {
		var opID string
		if opID, err = ids.NewUUID(); err == nil {
			var changed bool
			if changed, err = st.SetOwnAddress(want, opID); err == nil && changed {
				s.log.Info("the node's address records were updated", "address", want)
			}
		}
	}
	s.setAddrErr(err)
}

func (s *Service) setAddrErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil && (s.addrErr == nil || s.addrErr.Error() != err.Error()) {
		s.log.Error("cannot bring the node's address records in line with cluster.json", "error", err)
	}
	s.addrErr = err
}

func (s *Service) ensureListener() {
	want := s.cfg.PeerAddress()
	if want == "" {
		return
	}
	s.mu.Lock()
	open, t := s.listener != nil, s.transport
	s.mu.Unlock()
	if open {
		return
	}
	ap, err := netip.ParseAddrPort(want)
	var l *peerlistener.Listener
	if err == nil {
		var handle func(*tls.Conn, string)
		if t != nil {
			handle = t.Serve
		}
		l, err = peerlistener.Listen(peerlistener.Options{
			Address:     ap,
			Certificate: s.certificate,
			Accept:      s.acceptPin,
			Handle:      handle,
			Now:         s.now,
			Logger:      s.log,
		})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if s.listenErr == nil || s.listenErr.Error() != err.Error() {
			s.log.Error("cannot open the node-to-node listener; retrying", "address", want, "error", err)
		}
		s.listenErr = err
		return
	}
	s.listener, s.listenErr = l, nil
	s.log.Info("node-to-node listener open", "address", l.Addr().String())
}

// renew checks the certificate once a day (ADR 0013 B1).
func (s *Service) renew() {
	now := s.now()
	s.mu.Lock()
	due := s.lastRenew.IsZero() || now.Sub(s.lastRenew) >= 24*time.Hour
	if due {
		s.lastRenew = now
	}
	s.mu.Unlock()
	if !due {
		return
	}
	renewed, err := s.key.RenewIfDue(s.cfg.NodeID, now)
	if err != nil {
		s.log.Error("cannot renew the node certificate", "error", err)
	} else if renewed {
		s.log.Info("node certificate renewed with the same key", "not_after", s.key.NotAfter())
	}
	s.mu.Lock()
	s.renewErr = err
	s.mu.Unlock()
}

// Health implements localapi.Backend.
func (s *Service) Health(context.Context) models.Health {
	h := models.Health{Status: models.HealthOK, Version: version.Get().Version, UptimeSeconds: int64(s.now().Sub(s.started).Seconds())}
	st := s.getStore()
	stats := st.Stats()
	view := s.machine.View()
	s.mu.Lock()
	listening, listenErr, addrErr, renewErr, joinErr := s.listener != nil, s.listenErr, s.addrErr, s.renewErr, s.joinErr
	waits := make(map[string][]string, len(s.waits))
	for k, v := range s.waits {
		waits[k] = v
	}
	s.mu.Unlock()
	js := s.joinState()

	problem := ""
	if stats.LeaderID == "" {
		problem = "the cluster has no leader (this node is " + stats.State + ")"
	}
	h.Record("raft", true, problem)

	problem = ""
	if view.Stalled != nil {
		problem = fmt.Sprintf("stopped at log index %d: %s", view.Stalled.Index, view.Stalled.Reason)
	}
	h.Record("state_machine", true, problem)

	problem = ""
	if view.Stalled == nil && stats.LeaderID != "" && js == nil {
		mem, ok := s.machine.Member(s.cfg.NodeID)
		switch {
		case !ok:
			problem = "this node (" + s.cfg.NodeID + ") has no member entry in the replicated state"
		case mem.Pin != s.key.Pin():
			problem = "the node key on disk is not the one pinned in this node's member entry"
		}
	}
	h.Record("membership", true, problem)

	problem = ""
	if addrErr != nil {
		problem = addrErr.Error()
	}
	h.Record("address", true, problem)

	problem = ""
	if s.cfg.ListenAddress != "" && !listening {
		problem = "the node-to-node listener on " + s.cfg.PeerAddress() + " is not open"
		if listenErr != nil {
			problem += ": " + listenErr.Error()
		}
	}
	h.Record("listener", true, problem)

	problem = ""
	if renewErr != nil {
		problem = "certificate renewal failed: " + renewErr.Error()
	} else if s.key.NotAfter().Before(s.now()) {
		problem = "the node certificate expired on " + s.key.NotAfter().UTC().Format(time.RFC3339)
	}
	h.Record("certificate", false, problem)

	// A join in progress: the member being waited for (ADR 0013 B1,
	// join step 3), on every member.
	problem = ""
	for _, m := range view.Members {
		if m.State != clusterstate.StateJoining {
			continue
		}
		if names := waits[m.ID]; len(names) > 0 {
			problem = appendProblem(problem, fmt.Sprintf("the promotion of %s waits for %s", m.Name, joinNames(names)))
		} else {
			problem = appendProblem(problem, fmt.Sprintf("%s is joining", m.Name))
		}
	}
	h.Record("join", false, problem)

	// This node's own join, while it is joining.
	problem = ""
	if js != nil {
		switch js.Phase {
		case joinstate.PhaseRefused:
			problem = "the join was refused after its outcome was unknown: " + js.Message +
				"; the one-node state stays moved aside (" + js.Standalone + ") and this build cannot give up a join"
		case joinstate.PhaseCalling, joinstate.PhaseUnknown:
			problem = "the outcome of the join call is unknown; repeating it"
			if joinErr != nil {
				problem += ": " + joinErr.Error()
			}
		case joinstate.PhaseAccepted:
			problem = "joining cluster " + js.Admission.ClusterID
		}
	}
	h.Record("joining", js != nil && js.Phase == joinstate.PhaseRefused, problem)
	return h
}

func appendProblem(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

func joinNames(n []string) string {
	out := ""
	for i, x := range n {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}

// Status implements localapi.Backend.
func (s *Service) Status(context.Context) localapi.Status {
	st := s.getStore()
	view := s.machine.View()
	stats := st.Stats()
	s.mu.Lock()
	listening := s.listener != nil
	s.mu.Unlock()
	members := view.Members
	if members == nil {
		members = []clusterstate.Member{}
	}
	upToDate := view.Stalled == nil && stats.AppliedIndex >= stats.CommitIndex &&
		st.LibraryEntriesOnly(view.AppliedIndex, stats.AppliedIndex)
	v := version.Get()
	out := localapi.Status{
		NodeID:              s.cfg.NodeID,
		NodeName:            s.cfg.NodeName,
		ClusterID:           view.ClusterID,
		Pin:                 s.key.Pin(),
		CertificateNotAfter: s.key.NotAfter().UTC(),
		ListenAddress:       s.cfg.PeerAddress(),
		Listening:           listening,
		Raft:                stats,
		StateMachine:        localapi.StateMachineStatus{AppliedIndex: view.AppliedIndex, Stalled: view.Stalled, UpToDate: upToDate},
		Members:             members,
		Pendings:            view.Pendings,
		AcceptedBy:          s.acceptedNow().source,
		Build:               v.Version + "+" + v.Commit,
	}
	if servers, err := st.Servers(); err == nil {
		out.Servers = servers
	}
	if js := s.joinState(); js != nil {
		out.Join = &localapi.JoinStatus{Phase: js.Phase, ClusterID: js.Admission.ClusterID, Message: js.Message}
	}
	if out.Pendings == nil {
		out.Pendings = []clusterstate.Pending{}
	}
	return out
}

// RaftConfiguration implements localapi.Backend.
func (s *Service) RaftConfiguration(context.Context) (localapi.RaftConfiguration, error) {
	servers, err := s.getStore().Servers()
	if err != nil {
		return localapi.RaftConfiguration{}, err
	}
	if servers == nil {
		return localapi.RaftConfiguration{}, errors.New("the Raft configuration is empty")
	}
	return localapi.RaftConfiguration{Servers: servers}, nil
}
