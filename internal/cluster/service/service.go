// Package service is the running skep-cluster: it ties the node key, the
// store, the node-to-node listener and the local API's answers together
// (ADR 0013, step 8a).
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/peerlistener"
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
	SetOwnAddress(peerAddress, opID string) (bool, error)
}

// Options configures the service.
type Options struct {
	Config clusterconf.Config
	Key    *nodekey.Key
	Store  Store
	// Now defaults to time.Now.
	Now func() time.Time
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Interval is how often the background work runs; default 2 s.
	Interval time.Duration
}

// Service implements localapi.Backend.
type Service struct {
	cfg     clusterconf.Config
	key     *nodekey.Key
	store   Store
	now     func() time.Time
	log     *slog.Logger
	every   time.Duration
	started time.Time

	mu        sync.Mutex
	listener  *peerlistener.Listener
	listenErr error
	addrErr   error
	renewErr  error
	lastRenew time.Time
}

// New returns the service. Run starts its background work.
func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Interval <= 0 {
		o.Interval = 2 * time.Second
	}
	return &Service{cfg: o.Config, key: o.Key, store: o.Store, now: o.Now, log: o.Logger, every: o.Interval, started: o.Now()}
}

// Run does the background work until ctx ends: it keeps the node's own
// address records in line with cluster.json, opens the node-to-node
// listener when an address is configured, and renews the certificate.
// Without a listen address no network listener is ever opened.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(s.every)
	defer t.Stop()
	for {
		s.step()
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

func (s *Service) step() {
	s.reconcileAddress()
	s.ensureListener()
	s.renew()
}

// reconcileAddress applies a listen address written by "skep-cluster
// listen" to the membership entry and the Raft configuration.
func (s *Service) reconcileAddress() {
	if !s.store.IsLeader() {
		return
	}
	want := s.cfg.PeerAddress()
	mem, ok := s.store.Machine().Member(s.cfg.NodeID)
	servers, err := s.store.Servers()
	var raftAddr string
	if err == nil && store.Alone(servers, s.cfg.NodeID) {
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
			if changed, err = s.store.SetOwnAddress(want, opID); err == nil && changed {
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
	open := s.listener != nil
	s.mu.Unlock()
	if open {
		return
	}
	ap, err := netip.ParseAddrPort(want)
	var l *peerlistener.Listener
	if err == nil {
		l, err = peerlistener.Listen(peerlistener.Options{
			Address:     ap,
			Certificate: s.key.Certificate,
			Pinned:      s.store.Machine().PinnedNode,
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
	stats := s.store.Stats()
	view := s.store.Machine().View()
	s.mu.Lock()
	listening, listenErr, addrErr, renewErr := s.listener != nil, s.listenErr, s.addrErr, s.renewErr
	s.mu.Unlock()

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
	if view.Stalled == nil && stats.LeaderID != "" {
		mem, ok := s.store.Machine().Member(s.cfg.NodeID)
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
	return h
}

// Status implements localapi.Backend.
func (s *Service) Status(context.Context) localapi.Status {
	view := s.store.Machine().View()
	s.mu.Lock()
	listening := s.listener != nil
	s.mu.Unlock()
	members := view.Members
	if members == nil {
		members = []clusterstate.Member{}
	}
	v := version.Get()
	return localapi.Status{
		NodeID:              s.cfg.NodeID,
		NodeName:            s.cfg.NodeName,
		ClusterID:           view.ClusterID,
		Pin:                 s.key.Pin(),
		CertificateNotAfter: s.key.NotAfter().UTC(),
		ListenAddress:       s.cfg.PeerAddress(),
		Listening:           listening,
		Raft:                s.store.Stats(),
		StateMachine:        localapi.StateMachineStatus{AppliedIndex: view.AppliedIndex, Stalled: view.Stalled},
		Members:             members,
		Build:               v.Version + "+" + v.Commit,
	}
}

// RaftConfiguration implements localapi.Backend.
func (s *Service) RaftConfiguration(context.Context) (localapi.RaftConfiguration, error) {
	servers, err := s.store.Servers()
	if err != nil {
		return localapi.RaftConfiguration{}, err
	}
	if servers == nil {
		return localapi.RaftConfiguration{}, errors.New("the Raft configuration is empty")
	}
	return localapi.RaftConfiguration{Servers: servers}, nil
}
