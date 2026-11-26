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

	"github.com/greenbeanorg/skep/internal/cluster/audit"
	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joinstate"
	"github.com/greenbeanorg/skep/internal/cluster/layout"
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
	RemoveServer(id string) error
	TransferLeadership(id, address string) error
	Term() uint64
	Configuration() (servers []store.Server, index uint64, committed bool, err error)
	IsServer(id string, cached bool, maxAge time.Duration) bool
	LibraryEntriesOnly(from, to uint64) bool
	Barrier() error
	LeaderCh() <-chan bool
	Close() error
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
	// Removed is set when state/removed.json exists: the node was removed
	// from its cluster and runs only its local socket (decision D11).
	Removed *RemovedRecord
	// Now defaults to time.Now.
	Now func() time.Time
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Audit is the node's cluster audit, open for writing; nil writes
	// no local events (tests that do not look at it).
	Audit *audit.Log
	// Rotation is rotate.json as the start rule left it, or nil.
	Rotation *RotateState
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
	audit     *audit.Log

	mu        sync.Mutex
	store     Store
	transport *raftnet.Transport
	join      *joinstate.State
	listener  *peerlistener.Listener
	listenErr error
	addrErr   error
	renewErr  error
	joinErr   error
	lastRenew time.Time
	// barrierTerm is the term of the last successful barrier; the leader
	// repeats the barrier whenever the term differs (decision D5a).
	barrierTerm uint64
	nextLeader  time.Time
	nextStatus  time.Time
	// answers is the last status round: per member, its state machine's
	// applied index as it answered, or the error.
	answers map[string]statusResult
	// highest is, per member, the highest applied index it has ever
	// answered a status round with since this node started (decision
	// D12: a member that applied a removal is not named later).
	highest map[string]uint64
	// statusBusy is set while a status round runs in its goroutine.
	statusBusy bool
	// firstRound is set once the first status round since this service
	// started has ended; until then removal_applied says nothing (every
	// member would read "no answer since this node started").
	firstRound bool
	// keyRefused counts the consecutive status rounds in which a member
	// refused this node's key and none accepted it (D6 of step 8b-2b).
	keyRefused int
	// clocks is, per member, this node's last clock sample (D3).
	clocks map[string]clockSample
	// clockOp is the operation name of the clock request; a test sets an
	// unknown one to get the answer a build without it gives.
	clockOp string
	// seen is, per node, the pin its last successful connection to or
	// from this node used (D1).
	seen map[string]pinSeen
	// used is, per node, every pin seen on an accepted connection to or
	// from it since this service started (F4 of the 8b-2b review): once a
	// node has presented its new key, it has switched for good, whatever
	// a later connection with the old key says.
	used map[string]map[string]bool
	// rotation is this node's own key rotation (rotate.json), or nil.
	rotation *RotateState
	// nextRotate is when the rotation step next asks the leader.
	nextRotate time.Time
	// keyMu serialises the leader's handling of key-add and key-abort.
	keyMu sync.Mutex
	// rotMu serialises this node's switch decision with an abort's phase
	// write (no network call is made under it).
	rotMu sync.Mutex
	// afterRotateStep is a test hook, called after each step of this
	// node's rotation; returning true stops the node's rotation work
	// there (a crash point).
	afterRotateStep func(step string) bool
	// syncDir fsyncs a directory (layout.SyncDir; a test makes it fail).
	syncDir func(dir string) error
	// noticeBusy is set while a removed notice is being checked.
	noticeBusy bool
	// runCtx is Run's context, for work started outside the step loop.
	runCtx context.Context
	// removed is set once this node knows it was removed.
	removed *RemovedRecord
	// standaloneChecked is set once the start's clean-up of a leftover
	// moved-aside state has run (decision D2).
	standaloneChecked bool
	nextWaits         time.Time
	nextJoin          time.Time
	// waits names, per joining member, the members its promotion waits
	// for, as this node last saw it.
	waits map[string][]string
	// afterJoinStep is a test hook, called when this node, as leader,
	// has completed join step n for a member. Returning true freezes
	// the node's leader work, so that a test can stop it right there.
	afterJoinStep func(step int, nodeID string) bool
	// afterRemovalStep is the same for removal steps 0, 1 and 2.
	afterRemovalStep func(step int, nodeID string) bool
	frozen           bool
	// beforeBarrier is a test hook run before each barrier of a term
	// (leaderStep's and watchLeadership's).
	beforeBarrier func()
	// afterHandoverPass is a test hook run after each pass-on of a
	// leader's own removal to the new leader; the error it returns is
	// used in place of the pass's.
	afterHandoverPass func(err error) error
	// beforeStep is a test hook run at the start of each step.
	beforeStep func()
	// usersMu guards usersCh, closed and replaced whenever the users
	// change (the users-read long poll waits on it).
	usersMu sync.Mutex
	usersCh chan struct{}
}

// joinStepDone runs the test hook and reports whether the node is frozen.
func (s *Service) joinStepDone(step int, nodeID string) bool {
	s.mu.Lock()
	hook := s.afterJoinStep
	s.mu.Unlock()
	if hook == nil || !hook(step, nodeID) {
		return false
	}
	s.mu.Lock()
	s.frozen = true
	s.mu.Unlock()
	return true
}

// removalStepDone is joinStepDone for removal steps.
func (s *Service) removalStepDone(step int, nodeID string) bool {
	s.mu.Lock()
	hook := s.afterRemovalStep
	s.mu.Unlock()
	if hook == nil || !hook(step, nodeID) {
		return false
	}
	s.mu.Lock()
	s.frozen = true
	s.mu.Unlock()
	return true
}

func (s *Service) isFrozen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frozen
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
		join: o.Join, waits: map[string][]string{}, answers: map[string]statusResult{}, highest: map[string]uint64{}, removed: o.Removed,
		clocks: map[string]clockSample{}, clockOp: raftnet.OpClock, audit: o.Audit, seen: map[string]pinSeen{}, used: map[string]map[string]bool{}, rotation: o.Rotation,
		syncDir: layout.SyncDir}
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
	if m := st.Machine(); m != nil && m != s.machine {
		// Only before Run: the service and the store share one machine
		// in skep-cluster (cmd/skep-cluster serve passes the same one).
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
	s.mu.Lock()
	s.runCtx = ctx
	s.mu.Unlock()
	t := time.NewTicker(s.every)
	defer t.Stop()
	if st := s.getStore(); st != nil && !s.isRemoved() {
		go s.watchLeadership(ctx, st.LeaderCh())
	}
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
	s.mu.Lock()
	hook := s.beforeStep
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	if s.isRemoved() || s.getStore() == nil {
		return
	}
	s.ensureListener()
	s.revalidate()
	if s.checkRemoved() {
		return
	}
	s.cleanStandalone()
	s.joinStep(ctx)
	s.waitsStep(ctx)
	s.statusStep(ctx)
	s.leaderStep(ctx)
	s.rotateStep(ctx)
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

// OnApplied is installed as the state machine's hook. A membership change
// also brings the next status round forward, so that health names the
// members that have not applied a removal from the start.
func (s *Service) OnApplied() {
	s.mu.Lock()
	s.nextStatus = time.Time{}
	s.mu.Unlock()
	s.revalidate()
}

// reconcileAddress applies a listen address written by "skep-cluster
// listen" or "join-request" (or removed by "listen -none") to the
// membership entry and the Raft configuration of a one-node cluster. It is
// one step of the leader loop (decision D5b) and reports whether it changed
// anything.
func (s *Service) reconcileAddress() bool {
	st := s.getStore()
	want := s.cfg.PeerAddress()
	mem, ok := s.machine.Member(s.cfg.NodeID)
	servers, err := st.Servers()
	if err == nil && !store.Alone(servers, s.cfg.NodeID) {
		// A clustered node's address is not changed this way.
		if ok && mem.Address != want {
			err = fmt.Errorf("cluster.json says %q but the member entry says %q; the address of a clustered node cannot be changed in this build", want, mem.Address)
		}
		s.setAddrErr(err)
		return false
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
		return false
	}
	if err == nil && !ok {
		err = fmt.Errorf("node %s has no member entry in the replicated state", s.cfg.NodeID)
	}
	changed := false
	if err == nil {
		var opID string
		if opID, err = ids.NewUUID(); err == nil {
			if changed, err = st.SetOwnAddress(want, opID); err == nil && changed {
				s.log.Info("the node's address records were updated", "address", want)
				if want == "" {
					s.event(audit.EventListenCleared, nil)
				} else {
					s.event(audit.EventListenSet, map[string]string{"address": want})
				}
			}
		}
	}
	s.setAddrErr(err)
	return changed
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
	if want == "" || s.isRemoved() {
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
			handle = func(c *tls.Conn, pin string) {
				s.sawPinOf(pin)
				t.Serve(c, pin)
			}
		}
		l, err = peerlistener.Listen(peerlistener.Options{
			Address:     ap,
			Certificate: s.certificate,
			Accept:      s.acceptPin,
			Keep:        s.keepPin,
			CannotSay:   s.cannotSay,
			Group:       s.connGroup,
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
	if r := s.removedRecord(); r != nil {
		next := "\"skep-cluster leave --wipe\""
		if s.joinState() != nil {
			// A joining node: abandon puts its one-node state back.
			next = "\"skep-cluster join --abandon\" (which puts this host's one-node cluster back)"
		}
		h.Record("removed", true, fmt.Sprintf("this node was removed from cluster %s (%s, %s): its Raft instance and its listener are shut. "+
			"Make sure it was removed on a member (\"skep-cluster remove %s\"), then run %s as root on this node",
			r.ClusterID, r.How, r.At, s.cfg.NodeName, next))
		return h
	}
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
		case mem.Pin != s.key.Pin() && (mem.NextPin == "" || mem.NextPin != s.key.Pin()):
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

	// Removals in progress, and the members that have not yet applied
	// the last removal (decision D12; ADR 0013 B2).
	problem = ""
	for _, p := range s.removalProgress(view) {
		problem = appendProblem(problem, p)
	}
	h.Record("removal", false, problem)
	problem = ""
	if names := s.unappliedRemoval(view); len(names) > 0 {
		problem = "members that have not yet applied the last removal: " + joinNames(names) +
			". A member that was down while a compromised node was removed must be wiped (\"skep-cluster leave --wipe\") before it returns"
	}
	h.Record("removal_applied", false, problem)

	// Every member that answers refuses this node's key (D6 of step
	// 8b-2b): a warning only; the node never enters removed mode for it.
	problem = ""
	s.mu.Lock()
	refusedRounds := s.keyRefused
	s.mu.Unlock()
	if refusedRounds >= keyRefusedRounds {
		next := "\"skep-cluster leave --wipe\""
		if js != nil {
			next = "\"skep-cluster join --abandon\""
		}
		problem = fmt.Sprintf("every member that answers refuses this node's key (%d status rounds in a row); it was probably removed while it was down. Run %s as root",
			refusedRounds, next)
	}
	h.Record("key_refused", false, problem)

	// Key rotations in progress and what each waits for (D1 of step
	// 8b-2b), on every member.
	problem = ""
	for _, p := range s.rotationProgress(view) {
		problem = appendProblem(problem, p)
	}
	h.Record("key_rotation", false, problem)

	// Clock offsets from this node's point of view (D3 of step 8b-2b).
	problem, critical := s.clockProblem(view.Members)
	h.Record("clock", critical, problem)

	// This node's own join, while it is joining.
	problem = ""
	if js != nil {
		switch js.Phase {
		case joinstate.PhaseRefused:
			problem = "the join was refused after its outcome was unknown: " + js.Message +
				"; the one-node state stays moved aside (" + js.Standalone + "); to give the join up, remove this node on a member " +
				"if it is listed there, then run \"skep-cluster join --abandon\" as root"
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
	v := version.Get()
	if r := s.removedRecord(); r != nil {
		out := localapi.Status{NodeID: s.cfg.NodeID, NodeName: s.cfg.NodeName, ClusterID: r.ClusterID, Pin: s.key.Pin(),
			CertificateNotAfter: s.key.NotAfter().UTC(), ListenAddress: s.cfg.PeerAddress(), AcceptedBy: "none (removed)",
			Raft: store.Stats{State: "shutdown"}, Members: []clusterstate.Member{}, Pendings: []clusterstate.Pending{},
			Removed: &localapi.RemovedStatus{ClusterID: r.ClusterID, At: r.At, How: r.How}, Build: v.Version + "+" + v.Commit}
		if js := s.joinState(); js != nil {
			out.Join = &localapi.JoinStatus{Phase: js.Phase, ClusterID: js.Admission.ClusterID, Message: js.Message}
		}
		return out
	}
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
		Removals:            s.removalProgress(view),
		UnappliedRemoval:    s.unappliedRemoval(view),
		Clocks:              s.clockStatus(view.Members),
		Rotation:            s.rotationStatus(),
		Rotations:           s.rotationProgress(view),
		KeysSeen:            s.keysSeen(view),
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
	if s.isRemoved() || s.getStore() == nil {
		return localapi.RaftConfiguration{}, errors.New("this node was removed from its cluster")
	}
	servers, err := s.getStore().Servers()
	if err != nil {
		return localapi.RaftConfiguration{}, err
	}
	if servers == nil {
		return localapi.RaftConfiguration{}, errors.New("the Raft configuration is empty")
	}
	return localapi.RaftConfiguration{Servers: servers}, nil
}

// event writes a local event to the cluster audit. The step it records has
// already happened: a failed write is logged and changes nothing else.
func (s *Service) event(kind string, details map[string]string) {
	if s.audit == nil {
		return
	}
	if err := s.audit.Event(kind, details); err != nil {
		s.log.Error("cannot write a local event to the cluster audit", "kind", kind, "error", err)
	}
}
