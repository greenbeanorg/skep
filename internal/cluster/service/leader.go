package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
	"github.com/greenbeanorg/skep/internal/cluster/store"
)

// StatusAnswer is the answer of Skep's status RPC: the state machine's own
// applied index (ADR 0013 amendment 1), never the library's.
type StatusAnswer struct {
	NodeID       string `json:"node_id"`
	AppliedIndex uint64 `json:"applied_index"`
	Stalled      bool   `json:"stalled"`
}

// leaderStep is the leader's work, when it gains leadership and every
// LeaderInterval (ADR 0013 B1, "Order and crash points"): propose the
// expiry of due pending entries and finish the joins of members in state
// "joining".
func (s *Service) leaderStep(ctx context.Context) {
	if s.isFrozen() {
		return
	}
	st := s.getStore()
	isLeader := st.IsLeader()
	now := s.now()
	s.mu.Lock()
	gained := isLeader && !s.wasLeader
	s.wasLeader = isLeader
	due := isLeader && (gained || !now.Before(s.nextLeader))
	if due {
		s.nextLeader = now.Add(s.leaderInt)
	}
	s.mu.Unlock()
	if !due {
		return
	}
	if gained {
		// Act on everything committed before this term.
		if err := st.Barrier(); err != nil {
			s.log.Warn("leader: barrier failed; the work waits for the next round", "error", err)
			return
		}
		s.log.Info("leader: this node is the leader; checking unfinished joins and due expiries")
	}
	view := s.machine.View()
	if view.Stalled != nil {
		return
	}
	s.proposeExpiries(view)
	servers, err := st.Servers()
	if err != nil {
		s.log.Warn("leader: cannot read the Raft configuration", "error", err)
		return
	}
	for _, m := range view.Members {
		if m.State != clusterstate.StateJoining {
			continue
		}
		s.continueJoin(ctx, view, servers, m)
	}
}

// proposeExpiries proposes one expiry entry per pending entry whose expiry
// time the leader's clock has passed.
func (s *Service) proposeExpiries(view clusterstate.View) {
	now := s.now().Unix()
	for _, p := range view.Pendings {
		if now < p.ExpiresAt {
			continue
		}
		opID := "expire:" + p.NodeID + ":" + strconv.FormatInt(p.ExpiresAt, 10)
		e, err := clusterstate.NewEntry(clusterstate.TypePendingExpire, opID, clusterstate.ActorSystem, s.cfg.NodeID,
			clusterstate.PendingExpire{NodeID: p.NodeID, ExpiresAt: p.ExpiresAt, At: now})
		if err == nil {
			_, err = s.getStore().Apply(e)
		}
		if err != nil {
			s.log.Warn("leader: proposing a pending entry's expiry failed; retrying in the next round", "node_id", p.NodeID, "error", err)
		}
	}
}

func findServer(servers []store.Server, id string) (store.Server, bool) {
	for _, sv := range servers {
		if sv.ID == id {
			return sv, true
		}
	}
	return store.Server{}, false
}

// continueJoin does the next join step for a member in state joining.
func (s *Service) continueJoin(ctx context.Context, view clusterstate.View, servers []store.Server, m clusterstate.Member) {
	st := s.getStore()
	sv, ok := findServer(servers, m.ID)
	switch {
	case !ok:
		// Step 2.
		if err := st.AddNonvoter(m.ID, m.Address); err != nil {
			s.log.Warn("leader: join step 2 (add non-voter) failed; retrying", "node_id", m.ID, "error", err)
			return
		}
		s.log.Info("cluster membership: non-voter added", "event", "non-voter added", "node_id", m.ID, "name", m.Name,
			"actor", clusterstate.ActorSystem, "op_id", m.JoinOp, "origin", s.cfg.NodeID)
		s.joinStepDone(2, m.ID)
	case !sv.Voter:
		// Step 3: caught up, and every member has applied step 1.
		waits := s.promotionWaits(ctx, view, m)
		s.setWaits(m.ID, waits)
		if len(waits) > 0 {
			return
		}
		if err := st.AddVoter(m.ID, m.Address); err != nil {
			s.log.Warn("leader: join step 3 (promotion) failed; retrying", "node_id", m.ID, "error", err)
			return
		}
		s.log.Info("cluster membership: promoted", "event", "promoted", "node_id", m.ID, "name", m.Name,
			"actor", clusterstate.ActorSystem, "op_id", m.JoinOp, "origin", s.cfg.NodeID)
		if s.joinStepDone(3, m.ID) {
			return
		}
		fallthrough
	default:
		// Step 4.
		opID := "promoted:" + m.ID + ":" + strconv.FormatUint(m.JoinedIndex, 10)
		e, err := clusterstate.NewEntry(clusterstate.TypeMemberPromoted, opID, clusterstate.ActorSystem, s.cfg.NodeID,
			clusterstate.MemberPromoted{NodeID: m.ID})
		if err == nil {
			_, err = st.Apply(e)
		}
		if err != nil {
			s.log.Warn("leader: join step 4 failed; retrying", "node_id", m.ID, "error", err)
			return
		}
		s.joinStepDone(4, m.ID)
	}
}

func (s *Service) setWaits(id string, waits []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(waits) == 0 {
		delete(s.waits, id)
		return
	}
	s.waits[id] = waits
}

// waitsStep refreshes, on every node, which members each joining member's
// promotion waits for, so that health on every member names them.
func (s *Service) waitsStep(ctx context.Context) {
	now := s.now()
	s.mu.Lock()
	due := !now.Before(s.nextWaits)
	if due {
		s.nextWaits = now.Add(s.leaderInt)
	}
	s.mu.Unlock()
	if !due {
		return
	}
	view := s.machine.View()
	joining := map[string]bool{}
	for _, m := range view.Members {
		if m.State != clusterstate.StateJoining {
			continue
		}
		joining[m.ID] = true
		if s.getStore().IsLeader() {
			continue // the leader refreshes it in its own step
		}
		s.setWaits(m.ID, s.promotionWaits(ctx, view, m))
	}
	s.mu.Lock()
	for id := range s.waits {
		if !joining[id] {
			delete(s.waits, id)
		}
	}
	s.mu.Unlock()
}

// promotionWaits returns the members the promotion of m waits for (ADR 0013
// B1, join step 3, amended): m must have caught up (its state machine's
// applied index within one snapshot threshold of this node's commit index,
// and not stopped), and every member in the Skep membership, m and this
// node included, must have applied the entry that created m's member entry.
// A member that does not answer, or whose state machine is stopped, is
// waited for.
func (s *Service) promotionWaits(ctx context.Context, view clusterstate.View, m clusterstate.Member) []string {
	commit := s.getStore().Stats().CommitIndex
	var waits []string
	for _, x := range view.Members {
		applied, stalled, err := s.appliedOf(ctx, x)
		reason := ""
		switch {
		case err != nil:
			reason = "no answer"
		case stalled:
			reason = "state machine stopped"
		case applied < m.JoinedIndex:
			reason = fmt.Sprintf("applied %d, needs %d", applied, m.JoinedIndex)
		case x.ID == m.ID && applied+store.SnapshotThreshold < commit:
			reason = fmt.Sprintf("catching up, applied %d of %d", applied, commit)
		}
		if reason != "" {
			waits = append(waits, x.Name+" ("+reason+")")
		}
	}
	return waits
}

// appliedOf asks a member for its state machine's applied index.
func (s *Service) appliedOf(ctx context.Context, m clusterstate.Member) (uint64, bool, error) {
	if m.ID == s.cfg.NodeID {
		applied, stalled := s.machine.AppliedIndex()
		return applied, stalled, nil
	}
	if m.Address == "" {
		return 0, false, errors.New("no address")
	}
	cctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	var ans StatusAnswer
	if err := s.caller.Call(cctx, m.Address, m.Pin, raftnet.OpStatus, struct{}{}, &ans); err != nil {
		return 0, false, err
	}
	if ans.NodeID != m.ID {
		return 0, false, fmt.Errorf("node at %s answered as %s", m.Address, ans.NodeID)
	}
	return ans.AppliedIndex, ans.Stalled, nil
}
