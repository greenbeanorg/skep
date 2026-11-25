package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joinstate"
	"github.com/greenbeanorg/skep/internal/cluster/layout"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
	"github.com/greenbeanorg/skep/internal/ids"
)

// RemovedRecord is state/removed.json: this node learned that it was
// removed from its cluster (decision D11 of step 8b-2a). The service
// writes it, as the service account, before it shuts its Raft instance and
// its listener; at every later start it runs only its local socket.
// "leave --wipe" deletes it with the rest of state/.
type RemovedRecord struct {
	Format    int    `json:"format"`
	ClusterID string `json:"cluster_id"`
	NodeID    string `json:"node_id"`
	// At is when this node learned it (its own clock, RFC 3339).
	At string `json:"at"`
	// How says how it learned it: "notice from <name>, confirmed by <n>
	// members refusing this key" or
	// "its own log: state removing and no longer in the Raft
	// configuration" or "its own log: no member entry".
	How string `json:"how"`
}

const removedFormat = 1

// ReadRemoved reads state/removed.json in dataDir; (nil, nil) without one.
func ReadRemoved(dataDir string) (*RemovedRecord, error) {
	path := filepath.Join(layout.State(dataDir), layout.RemovedFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) > 64<<10 {
		return nil, fmt.Errorf("%s is too large", path)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var r RemovedRecord
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if r.Format != removedFormat || !clusterstate.ValidNodeID(r.NodeID) {
		return nil, fmt.Errorf("%s: format or node ID not valid", path)
	}
	return &r, nil
}

// writeRemoved writes state/removed.json atomically: a temporary file,
// fsync, rename, fsync of state/.
func writeRemoved(dataDir string, r RemovedRecord) error {
	r.Format = removedFormat
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	dir := layout.State(dataDir)
	if err := layout.MakeState(dataDir); err != nil {
		return err
	}
	path := filepath.Join(dir, layout.RemovedFile)
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

func (s *Service) isRemoved() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.removed != nil
}

func (s *Service) removedRecord() *RemovedRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removed == nil {
		return nil
	}
	c := *s.removed
	return &c
}

// checkRemoved finds out from this node's own log whether it was removed:
// its own member entry is "removing" and it is no longer a server in its
// latest Raft configuration, or its member entry is gone. Only once the
// state machine has caught up with the applied-membership file, and not
// while it is joining and not yet listed. It reports whether the node is
// (now) removed.
func (s *Service) checkRemoved() bool {
	if s.isRemoved() {
		return true
	}
	a := s.acceptedNow()
	if a.source != "state machine" {
		return false
	}
	view := s.machine.View()
	if view.Stalled != nil || view.ClusterID == "" {
		return false
	}
	self, ok := memberOf(view, s.cfg.NodeID)
	how := ""
	switch {
	case !ok && s.joinState() == nil:
		how = "its own log: no member entry for this node"
	case ok && self.State == clusterstate.StateRemoving:
		st := s.getStore()
		if st.IsLeader() {
			return false // it hands the leadership over first
		}
		servers, _, _, err := st.Configuration()
		if err != nil || servers == nil {
			return false
		}
		if _, in := findServer(servers, s.cfg.NodeID); in {
			return false
		}
		how = "its own log: state removing and no longer in the Raft configuration"
	default:
		return false
	}
	s.enterRemoved(view.ClusterID, how)
	return true
}

// enterRemoved writes the record, then shuts the listener and the Raft
// instance. The local socket stays for status and health.
func (s *Service) enterRemoved(clusterID, how string) {
	rec := RemovedRecord{ClusterID: clusterID, NodeID: s.cfg.NodeID, At: s.now().UTC().Format(time.RFC3339), How: how}
	s.mu.Lock()
	if s.removed != nil {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	if err := writeRemoved(s.dataDir, rec); err != nil {
		// Not durable: stay as it is and try again next round, rather
		// than act on something a restart would forget.
		s.log.Error("this node was removed from its cluster, but the record could not be written; retrying", "error", err)
		return
	}
	s.mu.Lock()
	s.removed = &rec
	l := s.listener
	s.listener = nil
	st := s.store
	s.mu.Unlock()
	s.log.Error("this node was removed from its cluster: its Raft instance and its listener are shut; run \"skep-cluster leave --wipe\"",
		"cluster_id", clusterID, "how", how)
	if l != nil {
		_ = l.Close()
	}
	if st != nil {
		go func() { _ = st.Close() }() // not on the caller's goroutine: an RPC handler may be inside the library
	}
}

// notifyRemoved sends the removed node a best-effort notice after the
// final entry (ADR 0013 B1: the removed node is not told by Raft).
func (s *Service) notifyRemoved(ctx context.Context, clusterID string, m clusterstate.Member) {
	if m.Address == "" || m.ID == s.cfg.NodeID {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	err := s.caller.Call(cctx, m.Address, m.Pin, raftnet.OpRemoved, RemovedNotice{ClusterID: clusterID, NodeID: m.ID}, nil)
	if err != nil {
		s.log.Info("the notice to the removed node was not delivered (best effort)", "node_id", m.ID, "address", m.Address, "error", err)
		return
	}
	s.log.Info("the removed node was told", "node_id", m.ID, "address", m.Address)
}

// RemovedNotice is the body of the removed notice.
type RemovedNotice struct {
	ClusterID string `json:"cluster_id"`
	NodeID    string `json:"node_id"`
}

// The check of a removed notice: up to noticeRounds rounds of probes,
// noticePause apart (the other members apply the final entry a moment
// after the leader does), each probe bounded by noticeProbeTimeout.
const (
	noticeRounds       = 3
	noticePause        = 2 * time.Second
	noticeProbeTimeout = 3 * time.Second
)

// handleRemovedNotice: the transport has checked that the sender's key is
// a member key this node accepts. The notice must name this node and its
// cluster. A valid notice is not believed on the sender's word: it only
// starts a check (checkNotice), off the RPC's goroutine, one at a time.
func (s *Service) handleRemovedNotice(from raftnet.Peer, n RemovedNotice) (any, error) {
	clusterID := s.machine.View().ClusterID
	if js := s.joinState(); js != nil && clusterID == "" {
		clusterID = js.Admission.ClusterID
	}
	if n.NodeID != s.cfg.NodeID || n.ClusterID == "" || n.ClusterID != clusterID {
		return nil, &raftnet.PeerError{Code: raftnet.CodeRefused, Message: "the notice is not for this node in this cluster"}
	}
	s.mu.Lock()
	busy := s.noticeBusy
	s.noticeBusy = true
	ctx := s.runCtx
	s.mu.Unlock()
	if busy {
		s.log.Info("a removed notice arrived while another is being checked; it is dropped", "from", from.Name, "node_id", from.NodeID)
		return struct{}{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	go s.checkNotice(ctx, clusterID, from)
	return struct{}{}, nil
}

// checkNotice believes a removed notice only when this node's own key is
// refused: it probes, with its own key, every other node it accepts now,
// the sender included, and enters removed mode only if at least one
// answers and none that answers accepts the key. Otherwise it tries again
// after noticePause, up to noticeRounds rounds, then drops the notice.
func (s *Service) checkNotice(ctx context.Context, clusterID string, from raftnet.Peer) {
	defer func() {
		s.mu.Lock()
		s.noticeBusy = false
		s.mu.Unlock()
	}()
	for round := 1; round <= noticeRounds; round++ {
		if round > 1 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(noticePause):
			}
		}
		if s.isRemoved() || ctx.Err() != nil {
			return
		}
		refused, accepted := s.probeOwnKey(from)
		if refused > 0 && len(accepted) == 0 {
			s.enterRemoved(clusterID, fmt.Sprintf("notice from %s, confirmed by %d members refusing this key", from.Name, refused))
			return
		}
		s.log.Info("a removed notice is not confirmed yet", "from", from.Name, "round", round, "refused_by", refused, "accepted_by", joinNames(accepted))
	}
	s.log.Warn("a removed notice was dropped: no round had a node refuse this node's key with none accepting it",
		"from", from.Name, "node_id", from.NodeID, "rounds", noticeRounds)
}

// probeOwnKey probes every node this node accepts now (not pending ones),
// and the sender, with this node's own key. It returns how many answered
// and refused the key, and the names of those that accept it.
func (s *Service) probeOwnKey(from raftnet.Peer) (int, []string) {
	refused := 0
	var accepted []string
	seen := map[string]bool{s.cfg.NodeID: true}
	for _, p := range append(s.acceptedNow().peers, from) {
		if p.Kind == raftnet.KindPending || seen[p.NodeID] || p.Address == "" {
			continue
		}
		seen[p.NodeID] = true
		switch res, _ := raftnet.ProbeKey(p.Address, p.Pin, s.certificate, noticeProbeTimeout); res {
		case raftnet.ProbeRefused:
			refused++
		case raftnet.ProbeAccepted:
			accepted = append(accepted, p.Name)
		}
	}
	return refused, accepted
}

// statusResult is one member's answer in the last status round.
type statusResult struct {
	applied uint64
	stalled bool
	err     error
	at      time.Time
}

// statusStep is the status round every node runs every LeaderInterval
// (and at once after an entry that removed a member): it asks every
// member for its state machine's applied index, so that health on every
// member names the members that have not yet applied a removal (decision
// D12; ADR 0013 B2).
func (s *Service) statusStep(ctx context.Context) {
	now := s.now()
	s.mu.Lock()
	if now.Before(s.nextStatus) || s.statusBusy {
		s.mu.Unlock()
		return
	}
	s.nextStatus = now.Add(s.leaderInt)
	s.statusBusy = true
	s.mu.Unlock()
	// In its own goroutine, so that the once-a-second work is not held
	// for statusTimeout while a member does not answer.
	members := s.machine.View().Members
	go func() {
		res := s.statusRound(ctx, members)
		s.mu.Lock()
		s.answers = res
		s.noteApplied(res)
		s.statusBusy = false
		s.mu.Unlock()
	}()
}

// noteApplied keeps, per member, the highest applied index it has ever
// answered with (since this node started). s.mu is held.
func (s *Service) noteApplied(res map[string]statusResult) {
	for id, r := range res {
		if h, ok := s.highest[id]; r.err == nil && (!ok || r.applied > h) {
			s.highest[id] = r.applied
		}
	}
}

// statusRound asks the given members at once.
func (s *Service) statusRound(ctx context.Context, members []clusterstate.Member) map[string]statusResult {
	out := map[string]statusResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, m := range members {
		wg.Add(1)
		go func(m clusterstate.Member) {
			defer wg.Done()
			applied, stalled, err := s.appliedOf(ctx, m)
			mu.Lock()
			out[m.ID] = statusResult{applied: applied, stalled: stalled, err: err, at: s.now()}
			mu.Unlock()
		}(m)
	}
	wg.Wait()
	return out
}

// unappliedRemoval returns the names of the members that have not been
// seen to apply the last removal, by the highest applied index each has
// ever answered with since this node started: a member that applied it
// and is down later is not named.
func (s *Service) unappliedRemoval(view clusterstate.View) []string {
	if view.RemovedIndex == 0 {
		return nil
	}
	s.mu.Lock()
	answers := s.answers
	highest := make(map[string]uint64, len(s.highest))
	for id, h := range s.highest {
		highest[id] = h
	}
	s.mu.Unlock()
	var out []string
	for _, m := range view.Members {
		if m.State == clusterstate.StateRemoving {
			continue // it is being removed itself
		}
		r, ok := answers[m.ID]
		hi, heard := highest[m.ID]
		if m.ID == s.cfg.NodeID {
			applied, stalled := s.machine.AppliedIndex()
			r, ok = statusResult{applied: applied, stalled: stalled}, true
			hi, heard = applied, true
		}
		switch {
		case heard && hi >= view.RemovedIndex:
		case !heard:
			out = append(out, m.Name+" (no answer since this node started)")
		case ok && r.err == nil && r.stalled:
			out = append(out, fmt.Sprintf("%s (applied %d, the removal is %d; state machine stopped)", m.Name, hi, view.RemovedIndex))
		default:
			out = append(out, fmt.Sprintf("%s (applied %d, the removal is %d)", m.Name, hi, view.RemovedIndex))
		}
	}
	return out
}

// removalProgress says, for each member being removed, what its removal
// waits for, as this node sees it.
func (s *Service) removalProgress(view clusterstate.View) []string {
	var out []string
	st := s.getStore()
	if st == nil {
		return nil
	}
	servers, _, committed, err := st.Configuration()
	s.mu.Lock()
	answers := s.answers
	s.mu.Unlock()
	for _, m := range view.Members {
		if m.State != clusterstate.StateRemoving {
			continue
		}
		switch {
		case err != nil:
			out = append(out, fmt.Sprintf("the removal of %s: the Raft configuration cannot be read (%v)", m.Name, err))
		case func() bool { _, in := findServer(servers, m.ID); return in }():
			out = append(out, fmt.Sprintf("the removal of %s waits for the leader to remove it from the Raft configuration", m.Name))
		case !committed:
			var missing []string
			for _, sv := range servers {
				if !sv.Voter || sv.ID == s.cfg.NodeID {
					continue
				}
				if r, ok := answers[sv.ID]; !ok || r.err != nil {
					name := sv.ID
					if mm, ok := memberOf(view, sv.ID); ok {
						name = mm.Name
					}
					missing = append(missing, name)
				}
			}
			msg := fmt.Sprintf("the removal of %s waits for the Raft configuration without it to commit", m.Name)
			if len(missing) > 0 {
				sort.Strings(missing)
				msg += "; it needs " + joinNames(missing) + ", which did not answer"
			}
			out = append(out, msg)
		default:
			out = append(out, fmt.Sprintf("the removal of %s waits for the leader's final entry", m.Name))
		}
	}
	return out
}

// keepPin is the listener's rule for open connections: accepted, and a
// removing member's connections are closed once it is no longer a server
// in the latest Raft configuration (decision D11).
func (s *Service) keepPin(pin string) bool {
	p, ok := s.ByPin(pin)
	if !ok {
		return false
	}
	if p.Kind == raftnet.KindRemoving {
		return s.RaftServer(p.NodeID, false)
	}
	return true
}

// cleanStandalone is the last clause of the start rule (decision D2): a
// node whose own state is member, that is not joining, and that still
// finds a moved-aside state deletes it, with a log line. It runs once,
// when the state machine has caught up.
func (s *Service) cleanStandalone() {
	s.mu.Lock()
	done := s.standaloneChecked
	s.mu.Unlock()
	if done || s.joinState() != nil || s.acceptedNow().source != "state machine" {
		return
	}
	mem, ok := s.machine.Member(s.cfg.NodeID)
	if !ok || mem.State != clusterstate.StateMember {
		return
	}
	aside, err := layout.Standalones(s.dataDir)
	if err != nil {
		return
	}
	for _, a := range aside {
		if err := joinstate.DeleteStandalone(s.dataDir, a); err != nil {
			s.log.Error("cannot delete a moved-aside one-node state left by a finished join", "directory", a, "error", err)
			return
		}
		s.log.Warn("this node is a member and not joining: a moved-aside one-node state left by a finished join was deleted", "directory", a)
	}
	s.mu.Lock()
	s.standaloneChecked = true
	s.mu.Unlock()
}

// forwardRemove is the body of a removal passed to the leader.
type forwardRemove struct {
	Name            string `json:"name"`
	Actor           string `json:"actor"`
	OpID            string `json:"op_id"`
	AllowUnanswered bool   `json:"allow_unanswered,omitempty"`
	ClusterID       string `json:"cluster_id,omitempty"`
}

func removedError() *localapi.OpError {
	return &localapi.OpError{HTTPStatus: http.StatusConflict, Code: "removed",
		Message: "this node was removed from its cluster; run \"skep-cluster leave --wipe\""}
}

// Remove implements localapi.Backend: "skep-cluster remove NAME", root
// only on this node's socket (decision D3). A member that is not the
// leader passes it to the leader. It returns when the first entry
// (removing) is committed; the leader loop does the other two steps.
func (s *Service) Remove(ctx context.Context, req localapi.RemoveRequest, actor string) (localapi.RemoveResult, error) {
	if s.isRemoved() {
		return localapi.RemoveResult{}, removedError()
	}
	if !clusterstate.ValidName(req.Name) {
		return localapi.RemoveResult{}, &localapi.OpError{HTTPStatus: http.StatusBadRequest, Code: "invalid_request", Message: "not a valid node name"}
	}
	if req.AllowUnanswered && !clusterstate.ValidNodeID(req.ClusterID) {
		return localapi.RemoveResult{}, &localapi.OpError{HTTPStatus: http.StatusBadRequest, Code: "invalid_request",
			Message: "allow_unanswered needs the cluster ID typed back"}
	}
	if s.joinState() != nil {
		return localapi.RemoveResult{}, &localapi.OpError{HTTPStatus: http.StatusConflict, Code: "joining",
			Message: "this node is joining a cluster; remove on a member"}
	}
	opID, err := ids.NewUUID()
	if err != nil {
		return localapi.RemoveResult{}, err
	}
	fw := forwardRemove{Name: req.Name, Actor: actor, OpID: opID, AllowUnanswered: req.AllowUnanswered, ClusterID: req.ClusterID}
	if s.getStore().IsLeader() {
		return s.removeAsLeader(ctx, fw, s.cfg.NodeID)
	}
	var res localapi.RemoveResult
	err = s.toLeader(ctx, raftnet.OpRemove, fw, &res)
	return res, err
}

func refusedErr(msg string) *localapi.OpError {
	return &localapi.OpError{HTTPStatus: http.StatusConflict, Code: "refused", Message: msg}
}

// removeAsLeader is the leader's side: the checks, the hand-over when the
// leader is the node to remove (D6), then removal step 0.
func (s *Service) removeAsLeader(ctx context.Context, req forwardRemove, origin string) (localapi.RemoveResult, error) {
	st := s.getStore()
	view := s.machine.View()
	if view.Stalled != nil {
		return localapi.RemoveResult{}, &localapi.OpError{HTTPStatus: http.StatusServiceUnavailable, Code: "unavailable",
			Message: "this node's state machine is stopped; nothing was written"}
	}
	var x clusterstate.Member
	found := false
	for _, m := range view.Members {
		if m.Name == req.Name {
			x, found = m, true
		}
	}
	if !found {
		return localapi.RemoveResult{}, &localapi.OpError{HTTPStatus: http.StatusNotFound, Code: "not_found",
			Message: "there is no member named " + req.Name}
	}
	if x.State == clusterstate.StateRemoving {
		return localapi.RemoveResult{}, refusedErr(x.Name + " is already being removed; the leader finishes it by itself")
	}
	// D7a, also checked at apply.
	for _, m := range view.Members {
		if m.ID != x.ID && m.State == clusterstate.StateJoining {
			return localapi.RemoveResult{}, refusedErr(fmt.Sprintf("%s is joining; %s cannot be removed until that join has finished or %s is removed", m.Name, x.Name, m.Name))
		}
	}
	if req.AllowUnanswered && req.ClusterID != view.ClusterID {
		return localapi.RemoveResult{}, refusedErr(fmt.Sprintf("the cluster ID typed back (%s) is not this cluster's (%s); nothing was written", req.ClusterID, view.ClusterID))
	}
	// D7c: without x, a majority of the remaining voters must have
	// answered a status round, which is made now.
	servers, _, _, err := st.Configuration()
	if err != nil {
		return localapi.RemoveResult{}, &localapi.OpError{HTTPStatus: http.StatusServiceUnavailable, Code: "unavailable", Message: err.Error()}
	}
	var remaining []clusterstate.Member
	var voterIDs []string
	for _, sv := range servers {
		if !sv.Voter || sv.ID == x.ID {
			continue
		}
		voterIDs = append(voterIDs, sv.ID)
		if m, ok := memberOf(view, sv.ID); ok {
			remaining = append(remaining, m)
		}
	}
	answers := s.statusRound(ctx, remaining)
	s.mu.Lock()
	for id, r := range answers {
		s.answers[id] = r
	}
	s.noteApplied(answers)
	s.mu.Unlock()
	var silent []string
	answered := 0
	for _, id := range voterIDs {
		r, ok := answers[id]
		if ok && r.err == nil {
			answered++
			continue
		}
		name := id
		if m, ok := memberOf(view, id); ok {
			name = m.Name
		}
		silent = append(silent, name)
	}
	need := len(voterIDs)/2 + 1
	if len(voterIDs) > 0 && answered < need && !req.AllowUnanswered {
		return localapi.RemoveResult{}, &localapi.OpError{HTTPStatus: http.StatusConflict, Code: "voters_unanswered",
			Message: fmt.Sprintf("without %s the cluster has %d voters and needs %d of them for every change, but only %d answered now (no answer from %s): "+
				"after this removal the cluster would accept no changes until they return. Nothing was written. "+
				"To remove it all the same, run the command again with -allow-unanswered and confirm the cluster ID",
				x.Name, len(voterIDs), need, answered, joinNames(silent))}
	}
	// D6: the leader is the node to remove: hand the leadership over
	// first; the new leader does the removal.
	if x.ID == s.cfg.NodeID {
		if err := st.TransferLeadership(); err != nil {
			return localapi.RemoveResult{}, &localapi.OpError{HTTPStatus: http.StatusServiceUnavailable, Code: "handover_failed",
				Message: "this node is the leader and must hand the leadership over before it is removed, and the hand-over failed: " + err.Error() + "; nothing was written"}
		}
		s.log.Info("leader: leadership handed over before this node's removal")
		deadline := time.Now().Add(10 * time.Second)
		for {
			id, _ := st.Leader()
			if id != "" && id != s.cfg.NodeID {
				break
			}
			if time.Now().After(deadline) {
				return localapi.RemoveResult{}, &localapi.OpError{HTTPStatus: http.StatusServiceUnavailable, Code: "handover_failed",
					Message: "the leadership was handed over, but no new leader is known after 10 s; nothing was written: run the command again"}
			}
			select {
			case <-ctx.Done():
				return localapi.RemoveResult{}, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		var res localapi.RemoveResult
		err := s.toLeader(ctx, raftnet.OpRemove, req, &res)
		if err == nil {
			res.HandedOver = true
		}
		return res, err
	}
	e, err := clusterstate.NewEntry(clusterstate.TypeMemberRemoving, req.OpID, req.Actor, origin,
		clusterstate.MemberRemoving{NodeID: x.ID, ForgetGuests: []string{}})
	if err != nil {
		return localapi.RemoveResult{}, err
	}
	if _, err := st.Apply(e); err != nil {
		return localapi.RemoveResult{}, applyError(err)
	}
	s.removalStepDone(0, x.ID)
	// The leader loop does the next step at once.
	s.mu.Lock()
	s.nextLeader = time.Time{}
	s.mu.Unlock()
	return localapi.RemoveResult{NodeID: x.ID, Name: x.Name, OpID: req.OpID, ClusterID: view.ClusterID, Unanswered: silent}, nil
}

// handleRemove is the leader's side of a removal passed by a member.
func (s *Service) handleRemove(ctx context.Context, from raftnet.Peer, body json.RawMessage) (any, error) {
	var req forwardRemove
	if err := strictJSON(body, &req); err != nil || !opIDRE.MatchString(req.OpID) || req.Actor == "" || !clusterstate.ValidName(req.Name) {
		return nil, &raftnet.PeerError{Code: raftnet.CodeInvalidRequest, Message: "the remove request is not valid"}
	}
	if hint, ok := s.notLeader(); ok {
		return nil, hint
	}
	res, err := s.removeAsLeader(ctx, req, from.NodeID)
	return res, toPeerError(err)
}

// decodeNotice reads a removed notice strictly.
func decodeNotice(body json.RawMessage) (RemovedNotice, error) {
	var n RemovedNotice
	dec := json.NewDecoder(io.LimitReader(bytes.NewReader(body), 4096))
	dec.DisallowUnknownFields()
	err := dec.Decode(&n)
	return n, err
}
