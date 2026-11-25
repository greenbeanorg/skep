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

// leaderStep is the leader's work (ADR 0013 B1, "Order and crash points";
// decision D5 of step 8b-2a). It is the only place that changes the Raft
// configuration (AddNonvoter, AddVoter, RemoveServer) or proposes the
// leader's own membership entries, and it does so one step at a time:
//   - whenever the Raft term differs from the term of the last successful
//     barrier, it makes a barrier first (D5a), and does nothing else until
//     one succeeds in the current term;
//   - each round reads the state after that barrier and does at most one
//     step (D5b); after a step the next round follows at the next tick,
//     otherwise after LeaderInterval.
func (s *Service) leaderStep(ctx context.Context) {
	if s.isFrozen() {
		return
	}
	st := s.getStore()
	if !st.IsLeader() {
		return
	}
	now := s.now()
	term := st.Term()
	s.mu.Lock()
	needBarrier := term != s.barrierTerm
	due := needBarrier || !now.Before(s.nextLeader)
	hook := s.beforeBarrier
	s.mu.Unlock()
	if !due {
		return
	}
	if needBarrier {
		if hook != nil {
			hook()
		}
		if !s.termBarrier(st, term) {
			return
		}
	}
	did := s.leaderRound(ctx)
	s.mu.Lock()
	if did {
		s.nextLeader = now // the next step at the next tick
	} else {
		s.nextLeader = now.Add(s.leaderInt)
	}
	s.mu.Unlock()
}

// termBarrier makes the barrier of term (D5a): it acts on everything
// committed before the term, and records barrierTerm only if the node is
// still the leader in that same term. Two callers may run it at once
// (leaderStep and watchLeadership); a barrier changes nothing, so a second
// one only waits again.
func (s *Service) termBarrier(st Store, term uint64) bool {
	if err := st.Barrier(); err != nil {
		s.log.Warn("leader: barrier failed; the work waits for the next round", "term", term, "error", err)
		return false
	}
	if st.Term() != term || !st.IsLeader() {
		return false // the term moved during the barrier: try again
	}
	s.mu.Lock()
	first := s.barrierTerm != term
	s.barrierTerm = term
	s.mu.Unlock()
	if first {
		s.log.Info("leader: this node is the leader; checking unfinished joins, removals and due expiries", "term", term)
	}
	return true
}

// watchLeadership makes the barrier of a new term as soon as the node
// becomes the leader (D12 of step 8c-1, as fixed by F4 of its review), in
// its own goroutine: Run's step can be held up for seconds by synchronous
// status calls, and a new leader answers the peer operations only after
// its barrier. The leader's work stays in leaderStep, one step per round;
// leaderStep skips its own barrier when this one has recorded the term.
func (s *Service) watchLeadership(ctx context.Context, ch <-chan bool) {
	for {
		select {
		case <-ctx.Done():
			return
		case leader := <-ch:
			st := s.getStore()
			if !leader || st == nil || s.isRemoved() || s.isFrozen() || !st.IsLeader() {
				continue
			}
			term := st.Term()
			s.mu.Lock()
			done := s.barrierTerm == term
			s.mu.Unlock()
			if !done {
				s.termBarrier(st, term)
			}
		}
	}
}

// leaderRound does at most one step and reports whether it did one.
func (s *Service) leaderRound(ctx context.Context) bool {
	if s.isFrozen() {
		return false
	}
	st := s.getStore()
	view := s.machine.View()
	if view.Stalled != nil || view.ClusterID == "" {
		return false
	}
	servers, _, committed, err := st.Configuration()
	if err != nil {
		s.log.Warn("leader: cannot read the Raft configuration", "error", err)
		return false
	}
	// A leader that is itself being removed hands leadership over first;
	// the new leader does the removal (ADR 0013 B1 removal step 1, D6).
	if self, ok := s.machine.Member(s.cfg.NodeID); ok && self.State == clusterstate.StateRemoving {
		s.mu.Lock()
		answers := s.answers
		s.mu.Unlock()
		target, ok := handoverTarget(view, servers, answers, s.cfg.NodeID)
		if !ok {
			s.log.Warn("leader: this node is being removed, and no voter in state member answered the last status round; the hand-over waits")
			return false
		}
		s.log.Info("leader: this node is being removed; handing the leadership over", "to", target.ID)
		if err := st.TransferLeadership(target.ID, target.Address); err != nil {
			s.log.Warn("leader: handing the leadership over failed; retrying", "to", target.ID, "error", err)
		}
		return true
	}
	// A configuration this leader changed that has not committed yet (a
	// change whose wait ran out): nothing else until it has (D5e).
	if !committed {
		return false
	}
	if store.Alone(servers, s.cfg.NodeID) && len(view.Members) == 1 && s.joinState() == nil {
		if s.reconcileAddress() {
			return true
		}
	} else {
		s.reconcileAddress() // reports a clustered node's mismatch
	}
	if s.ghostSweep(view, servers) {
		return true
	}
	if s.continueRemovals(ctx, view, servers) {
		return true
	}
	if s.proposeKeyDone(view) {
		return true
	}
	if s.proposeExpiries(view) {
		return true
	}
	for _, m := range view.Members {
		if m.State != clusterstate.StateJoining {
			continue
		}
		if s.continueJoin(ctx, view, servers, m) {
			return true
		}
	}
	return false
}

// ghostSweep removes a server of the Raft configuration that has no member
// entry (decision D5f), one per round, and logs it. Nothing creates such a
// server on purpose: a join writes the member entry before AddNonvoter,
// and a removal removes the server before the member entry.
func (s *Service) ghostSweep(view clusterstate.View, servers []store.Server) bool {
	for _, sv := range servers {
		if sv.ID == s.cfg.NodeID {
			continue
		}
		if _, ok := memberOf(view, sv.ID); ok {
			continue
		}
		if err := s.getStore().RemoveServer(sv.ID); err != nil {
			s.log.Warn("cluster membership: removing a Raft server without a member entry failed; it will be tried again", "event", "ghost removal failed",
				"node_id", sv.ID, "address", sv.Address, "voter", sv.Voter, "actor", clusterstate.ActorSystem, "origin", s.cfg.NodeID, "error", err.Error())
			return true
		}
		s.log.Warn("cluster membership: a Raft server without a member entry was removed", "event", "ghost removed",
			"node_id", sv.ID, "address", sv.Address, "voter", sv.Voter, "actor", clusterstate.ActorSystem, "origin", s.cfg.NodeID)
		return true
	}
	return false
}

// handoverTarget picks the voter a leader hands the leadership to before
// its own removal: a voter of the latest configuration other than this
// node whose member entry is in state member (never removing, never
// joining) and that answered the status round, with the highest applied
// index; on a tie the lowest node ID. ok is false when there is none.
func handoverTarget(view clusterstate.View, servers []store.Server, answers map[string]statusResult, self string) (store.Server, bool) {
	var best store.Server
	var bestApplied uint64
	found := false
	for _, sv := range servers {
		if !sv.Voter || sv.ID == self {
			continue
		}
		m, ok := memberOf(view, sv.ID)
		if !ok || m.State != clusterstate.StateMember {
			continue
		}
		r, ok := answers[sv.ID]
		if !ok || r.err != nil || r.stalled {
			continue
		}
		if !found || r.applied > bestApplied || (r.applied == bestApplied && sv.ID < best.ID) {
			best, bestApplied, found = sv, r.applied, true
		}
	}
	return best, found
}

func memberOf(view clusterstate.View, id string) (clusterstate.Member, bool) {
	for _, m := range view.Members {
		if m.ID == id {
			return m, true
		}
	}
	return clusterstate.Member{}, false
}

// continueRemovals does the next removal step for one member in state
// removing (ADR 0013 B1, removal; D5d, D5e): RemoveServer while it is a
// server in the latest configuration, voter or not; the final entry once
// the configuration without it is committed (the caller has checked that
// the latest configuration is committed).
func (s *Service) continueRemovals(ctx context.Context, view clusterstate.View, servers []store.Server) bool {
	st := s.getStore()
	for _, m := range view.Members {
		if m.State != clusterstate.StateRemoving {
			continue
		}
		if _, ok := findServer(servers, m.ID); ok {
			err := st.RemoveServer(m.ID)
			if err != nil {
				s.log.Warn("leader: removal step 1 (RemoveServer) did not complete; the next round reads the configuration again", "node_id", m.ID, "error", err)
				return true
			}
			s.log.Info("cluster membership: removed from the Raft configuration", "event", "server removed", "node_id", m.ID, "name", m.Name,
				"actor", clusterstate.ActorSystem, "origin", s.cfg.NodeID)
			s.removalStepDone(1, m.ID)
			return true
		}
		opID := "removed:" + m.ID + ":" + strconv.FormatUint(m.JoinedIndex, 10)
		e, err := clusterstate.NewEntry(clusterstate.TypeMemberRemoved, opID, clusterstate.ActorSystem, s.cfg.NodeID,
			clusterstate.MemberRemoved{NodeID: m.ID})
		if err == nil {
			_, err = st.Apply(e)
		}
		if err != nil {
			s.log.Warn("leader: removal step 2 failed; retrying", "node_id", m.ID, "error", err)
			return true
		}
		if s.removalStepDone(2, m.ID) {
			return true
		}
		s.notifyRemoved(ctx, view.ClusterID, m)
		return true
	}
	return false
}

// proposeExpiries proposes one expiry entry per pending entry whose expiry
// time the leader's clock has passed, and reports whether it proposed one.
func (s *Service) proposeExpiries(view clusterstate.View) bool {
	did := false
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
		did = true
	}
	return did
}

func findServer(servers []store.Server, id string) (store.Server, bool) {
	for _, sv := range servers {
		if sv.ID == id {
			return sv, true
		}
	}
	return store.Server{}, false
}

// continueJoin does the next join step for a member in state joining, one
// step per call, and reports whether it did one (or tried and failed).
func (s *Service) continueJoin(ctx context.Context, view clusterstate.View, servers []store.Server, m clusterstate.Member) bool {
	st := s.getStore()
	sv, ok := findServer(servers, m.ID)
	switch {
	case !ok:
		// Step 2.
		if err := st.AddNonvoter(m.ID, m.Address); err != nil {
			s.log.Warn("leader: join step 2 (add non-voter) failed; retrying", "node_id", m.ID, "error", err)
			return true
		}
		s.log.Info("cluster membership: non-voter added", "event", "non-voter added", "node_id", m.ID, "name", m.Name,
			"actor", clusterstate.ActorSystem, "op_id", m.JoinOp, "origin", s.cfg.NodeID)
		s.joinStepDone(2, m.ID)
		return true
	case !sv.Voter:
		// Step 3: caught up, and every member has applied step 1.
		waits := s.promotionWaits(ctx, view, m)
		s.setWaits(m.ID, waits)
		if len(waits) > 0 {
			return false
		}
		if err := st.AddVoter(m.ID, m.Address); err != nil {
			s.log.Warn("leader: join step 3 (promotion) failed; retrying", "node_id", m.ID, "error", err)
			return true
		}
		s.log.Info("cluster membership: promoted", "event", "promoted", "node_id", m.ID, "name", m.Name,
			"actor", clusterstate.ActorSystem, "op_id", m.JoinOp, "origin", s.cfg.NodeID)
		s.joinStepDone(3, m.ID)
		return true
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
			return true
		}
		s.joinStepDone(4, m.ID)
		return true
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
	if err := s.callMember(cctx, m, raftnet.OpStatus, struct{}{}, &ans); err != nil {
		return 0, false, err
	}
	if ans.NodeID != m.ID {
		return 0, false, fmt.Errorf("node at %s answered as %s", m.Address, ans.NodeID)
	}
	return ans.AppliedIndex, ans.Stalled, nil
}

// callMember makes a Skep RPC to a member, accepting either of its pins
// (a rotating member, D1 of step 8b-2b), and notes which pin its key had.
func (s *Service) callMember(ctx context.Context, m clusterstate.Member, op string, in, out any) error {
	pins := []string{m.Pin}
	if m.NextPin != "" {
		pins = append(pins, m.NextPin)
	}
	seen, err := s.caller.CallPins(ctx, m.Address, pins, op, in, out)
	if seen != "" {
		s.SawPin(m.ID, seen)
	}
	return err
}
