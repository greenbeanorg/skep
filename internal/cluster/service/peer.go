package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joincode"
	"github.com/greenbeanorg/skep/internal/cluster/joinstate"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
	"github.com/greenbeanorg/skep/internal/cluster/store"
	"github.com/greenbeanorg/skep/internal/ids"
)

var opIDRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// JoinRequest is the body of the join call.
type JoinRequest struct {
	OpID string `json:"op_id"`
}

// JoinAnswer is the join call's answer: the cluster and every current
// member entry, joining ones included (ADR 0013 amendment 1).
type JoinAnswer struct {
	ClusterID string          `json:"cluster_id"`
	Members   []joincode.Node `json:"members"`
}

// forwardAdmit is the body of a forwarded admit.
type forwardAdmit struct {
	Request string `json:"request"`
	Actor   string `json:"actor"`
	OpID    string `json:"op_id"`
}

// forwardWithdraw is the body of a forwarded withdrawal.
type forwardWithdraw struct {
	Name  string `json:"name"`
	Actor string `json:"actor"`
	OpID  string `json:"op_id"`
}

// HandlePeer implements raftnet.PeerHandler. The transport has checked
// that the caller's key may call op.
func (s *Service) HandlePeer(ctx context.Context, op string, from raftnet.Peer, body json.RawMessage) (any, error) {
	switch op {
	case raftnet.OpStatus:
		applied, stalled := s.machine.AppliedIndex()
		return StatusAnswer{NodeID: s.cfg.NodeID, AppliedIndex: applied, Stalled: stalled}, nil
	case raftnet.OpJoin:
		var req JoinRequest
		// The operation ID must be a lower-case UUID (a node ID's form).
		if err := strictJSON(body, &req); err != nil || !clusterstate.ValidNodeID(req.OpID) {
			return nil, &raftnet.PeerError{Code: raftnet.CodeInvalidRequest, Message: "the join request needs an operation ID that is a lower-case UUID"}
		}
		return s.handleJoin(from, req)
	case raftnet.OpAdmit:
		var req forwardAdmit
		if err := strictJSON(body, &req); err != nil || !opIDRE.MatchString(req.OpID) || req.Actor == "" {
			return nil, &raftnet.PeerError{Code: raftnet.CodeInvalidRequest, Message: "the admit request is not valid"}
		}
		if hint, ok := s.notLeader(); ok {
			return nil, hint
		}
		res, err := s.admitAsLeader(req.Request, req.Actor, from.NodeID, req.OpID)
		return res, toPeerError(err)
	case raftnet.OpWithdraw:
		var req forwardWithdraw
		if err := strictJSON(body, &req); err != nil || !opIDRE.MatchString(req.OpID) || req.Actor == "" {
			return nil, &raftnet.PeerError{Code: raftnet.CodeInvalidRequest, Message: "the withdraw request is not valid"}
		}
		if hint, ok := s.notLeader(); ok {
			return nil, hint
		}
		res, err := s.withdrawAsLeader(req.Name, req.Actor, from.NodeID, req.OpID)
		return res, toPeerError(err)
	}
	return nil, &raftnet.PeerError{Code: raftnet.CodeForbidden, Message: "unknown operation"}
}

func strictJSON(b json.RawMessage, v any) error {
	dec := json.NewDecoder(bytesReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("more than one JSON value")
	}
	return nil
}

func toPeerError(err error) error {
	if err == nil {
		return nil
	}
	var oe *localapi.OpError
	if errors.As(err, &oe) {
		return &raftnet.PeerError{Code: oe.Code, Message: oe.Message}
	}
	return &raftnet.PeerError{Code: raftnet.CodeUnavailable, Message: err.Error()}
}

// notLeader returns a not_leader answer naming the leader, when this node
// is not the leader.
func (s *Service) notLeader() (*raftnet.PeerError, bool) {
	st := s.getStore()
	if st.IsLeader() {
		return nil, false
	}
	id, addr := st.Leader()
	if id == "" {
		return &raftnet.PeerError{Code: raftnet.CodeUnavailable, Message: "the cluster has no leader now"}, true
	}
	return &raftnet.PeerError{Code: raftnet.CodeNotLeader, Message: "this node is not the leader",
		Leader: &raftnet.LeaderHint{NodeID: id, Address: addr}}, true
}

func (s *Service) joinAnswer() JoinAnswer {
	view := s.machine.View()
	a := JoinAnswer{ClusterID: view.ClusterID, Members: []joincode.Node{}}
	for _, m := range view.Members {
		a.Members = append(a.Members, joincode.Node{NodeID: m.ID, Name: m.Name, Address: m.Address, Pin: m.Pin})
	}
	return a
}

// handleJoin is the leader's side of the join call: join step 1 (ADR 0013
// B1). A repeat with the same operation ID, or from a key that already has
// its member entry, is answered as accepted without writing anything.
func (s *Service) handleJoin(from raftnet.Peer, req JoinRequest) (any, error) {
	if s.isFrozen() {
		return nil, &raftnet.PeerError{Code: raftnet.CodeUnavailable, Message: "this node is stopping"}
	}
	if hint, ok := s.notLeader(); ok {
		return nil, hint
	}
	if mem, ok := s.machine.Member(from.NodeID); ok {
		if mem.Pin != from.Pin {
			return nil, &raftnet.PeerError{Code: raftnet.CodeRefused, Message: "this node ID is a member with another key"}
		}
		return s.joinAnswer(), nil
	}
	view := s.machine.View()
	var pending *clusterstate.Pending
	for i := range view.Pendings {
		if view.Pendings[i].NodeID == from.NodeID {
			pending = &view.Pendings[i]
		}
	}
	now := s.now().Unix()
	switch {
	case pending == nil:
		return nil, &raftnet.PeerError{Code: raftnet.CodeRefused, Message: "this node has no pending entry"}
	case now >= pending.ExpiresAt:
		return nil, &raftnet.PeerError{Code: raftnet.CodeRefused, Message: "the pending entry of this node has expired"}
	}
	e, err := clusterstate.NewEntry(clusterstate.TypeMemberJoin, joinOpID(from.NodeID, req.OpID), "node:"+from.NodeID, s.cfg.NodeID,
		clusterstate.MemberJoin{NodeID: from.NodeID, Pin: from.Pin, At: now})
	if err != nil {
		return nil, &raftnet.PeerError{Code: raftnet.CodeInvalidRequest, Message: err.Error()}
	}
	res, err := s.getStore().Apply(e)
	switch {
	case errors.Is(err, store.ErrNotLeader):
		if hint, ok := s.notLeader(); ok {
			return nil, hint
		}
		return nil, &raftnet.PeerError{Code: raftnet.CodeUnavailable, Message: err.Error()}
	case errors.Is(err, clusterstate.ErrRefused):
		return nil, &raftnet.PeerError{Code: raftnet.CodeRefused, Message: err.Error()}
	case err != nil:
		return nil, &raftnet.PeerError{Code: raftnet.CodeOutcomeUnknown, Message: err.Error()}
	}
	// Accepted only if this node's member entry with this key exists.
	if mem, ok := s.machine.Member(from.NodeID); !ok || mem.Pin != from.Pin {
		return nil, &raftnet.PeerError{Code: raftnet.CodeRefused, Message: "the join was written, but there is no member entry for this node with this key"}
	}
	if !res.Duplicate {
		s.joinStepDone(1, from.NodeID)
	}
	return s.joinAnswer(), nil
}

// joinOpID is the operation ID of the member.join entry of a join call.
func joinOpID(nodeID, callOpID string) string { return "join:" + nodeID + ":" + callOpID }

// Join outcomes.
const (
	// JoinAccepted: a leader answered that join step 1 is applied.
	JoinAccepted = "accepted"
	// JoinRefused: a leader refused the call; this call applied nothing.
	JoinRefused = "refused"
	// JoinUnknown: a request was sent and no answer came.
	JoinUnknown = "unknown"
	// JoinUnreached: no member accepted the request (every handshake
	// failed, nothing was sent, or every member answered without a
	// leader); this call applied nothing.
	JoinUnreached = "unreached"
)

// JoinOutcome is the result of JoinCall.
type JoinOutcome struct {
	Result string
	Answer *JoinAnswer
	// Message is the leader's refusal, or a summary of the attempts.
	Message  string
	Attempts []string
}

// JoinCall makes the join call with the joining node's key: to the members
// of the admission and of earlier answers only, following a redirect to the
// leader only when it names one of them at its known address (ADR 0013 B1;
// D8 of the 8b-1 run). rounds is how many times every member is tried.
// An answer that fails checkAnswer makes the outcome unknown.
func JoinCall(ctx context.Context, caller raftnet.Caller, peers []joincode.Node, clusterID, opID string, rounds int, pause time.Duration, log *slog.Logger) JoinOutcome {
	known := map[string]joincode.Node{}
	for _, p := range peers {
		known[p.NodeID] = p
	}
	var out JoinOutcome
	unknown := false
	var refusal string
	for round := 0; round < rounds; round++ {
		for _, p := range peers {
			target := p
			for hops := 0; ; hops++ {
				cctx, cancel := context.WithTimeout(ctx, raftnet.PeerTimeout)
				var ans JoinAnswer
				err := caller.Call(cctx, target.Address, target.Pin, raftnet.OpJoin, JoinRequest{OpID: opID}, &ans)
				cancel()
				if err == nil {
					cerr := checkAnswer(ans, clusterID)
					if cerr == nil {
						out.Attempts = append(out.Attempts, target.Name+": accepted")
						out.Result, out.Answer = JoinAccepted, &ans
						return out
					}
					out.Attempts = append(out.Attempts, target.Name+" ("+target.Address+"): the answer is not used: "+cerr.Error())
					if log != nil {
						log.Warn("join call answer not used", "member", target.Name, "address", target.Address, "error", cerr.Error())
					}
					unknown = true
					break
				}
				out.Attempts = append(out.Attempts, target.Name+" ("+target.Address+"): "+err.Error())
				if log != nil {
					log.Info("join call attempt failed", "member", target.Name, "address", target.Address, "error", err.Error())
				}
				var pe *raftnet.PeerError
				var ou *raftnet.OutcomeUnknownError
				if errors.As(err, &ou) {
					unknown = true
					break
				}
				if !errors.As(err, &pe) {
					break // handshake refused or unreachable: nothing sent
				}
				if pe.Code == raftnet.CodeNotLeader && pe.Leader != nil && hops < 3 {
					l, ok := known[pe.Leader.NodeID]
					if ok && l.Address == pe.Leader.Address {
						target = l
						continue
					}
					out.Attempts = append(out.Attempts, fmt.Sprintf("redirect to %s at %s ignored: not a node of the admission", pe.Leader.NodeID, pe.Leader.Address))
				}
				switch pe.Code {
				case raftnet.CodeOutcomeUnknown:
					unknown = true
				case raftnet.CodeRefused:
					refusal = target.Name + ": " + pe.Message
				}
				break
			}
			if refusal != "" {
				break
			}
		}
		if refusal != "" {
			break
		}
		if round+1 < rounds {
			select {
			case <-ctx.Done():
				round = rounds
			case <-time.After(pause):
			}
		}
	}
	switch {
	case unknown:
		out.Result = JoinUnknown
		out.Message = "a join request was sent and no answer came"
	case refusal != "":
		out.Result, out.Message = JoinRefused, refusal
	default:
		out.Result = JoinUnreached
		out.Message = "no member of the admission accepted this node's key or had a leader"
	}
	return out
}

// checkAnswer: the admission's cluster, 1 to 64 nodes, each valid.
func checkAnswer(a JoinAnswer, clusterID string) error {
	if a.ClusterID != clusterID {
		return fmt.Errorf("the answer is for cluster %q, the admission for %s", a.ClusterID, clusterID)
	}
	if len(a.Members) == 0 || len(a.Members) > 64 {
		return fmt.Errorf("the answer lists %d members, not 1 to 64", len(a.Members))
	}
	return joincode.CheckNodes(a.Members)
}

// joinStep is a joining node's work: repeat a join call whose outcome is
// unknown, and finish the join once this node has applied a membership
// that lists it as a member.
func (s *Service) joinStep(ctx context.Context) {
	js := s.joinState()
	if js == nil {
		return
	}
	if mem, ok := s.machine.Member(s.cfg.NodeID); ok && mem.State == clusterstate.StateMember && s.acceptedNow().source == "state machine" {
		if err := joinstate.DeleteStandalone(s.dataDir, js.Standalone); err != nil {
			s.log.Error("join finished, but the moved-aside one-node state could not be deleted", "error", err)
			return
		}
		if err := joinstate.Remove(s.dataDir); err != nil {
			s.log.Error("join finished, but join.json could not be deleted", "error", err)
			return
		}
		s.mu.Lock()
		s.join = nil
		s.mu.Unlock()
		s.log.Info("join finished: this node is a member; the admission's pins are dropped and the moved-aside state is deleted",
			"cluster_id", js.Admission.ClusterID)
		return
	}
	if js.Phase != joinstate.PhaseCalling && js.Phase != joinstate.PhaseUnknown {
		return
	}
	now := s.now()
	s.mu.Lock()
	due := !now.Before(s.nextJoin)
	if due {
		s.nextJoin = now.Add(s.leaderInt)
	}
	s.mu.Unlock()
	if !due {
		return
	}
	out := JoinCall(ctx, s.caller, js.Peers(s.cfg.NodeID), js.Admission.ClusterID, js.OpID, 1, 0, s.log)
	switch out.Result {
	case JoinAccepted:
		js.Phase, js.Answer, js.Message = joinstate.PhaseAccepted, out.Answer.Members, ""
		s.log.Info("join call accepted after an unknown outcome", "cluster_id", out.Answer.ClusterID)
	case JoinRefused:
		js.Phase, js.Message = joinstate.PhaseRefused, out.Message
		s.log.Error("join call refused after an unknown outcome; the one-node state stays moved aside", "reason", out.Message)
	default:
		s.mu.Lock()
		s.joinErr = errors.New(out.Message)
		s.mu.Unlock()
		return
	}
	if err := joinstate.Write(s.dataDir, *js); err != nil {
		s.log.Error("cannot record the join's outcome", "error", err)
		return
	}
	s.mu.Lock()
	s.join, s.joinErr = js, nil
	s.mu.Unlock()
}

// Admit implements localapi.Backend: "skep-cluster admit <request>", root
// only on this node's socket. A member that is not the leader passes it to
// the leader, which checks it again; applying the entry checks it again on
// every node.
func (s *Service) Admit(ctx context.Context, request, actor string) (localapi.AdmitResult, error) {
	if s.cfg.ListenAddress == "" {
		return localapi.AdmitResult{}, &localapi.OpError{HTTPStatus: http.StatusConflict, Code: "no_listen_address",
			Message: "this node has no listen address: run \"skep-cluster listen -address ADDR\" first"}
	}
	if s.joinState() != nil {
		return localapi.AdmitResult{}, &localapi.OpError{HTTPStatus: http.StatusConflict, Code: "joining",
			Message: "this node is joining a cluster; admit on a member"}
	}
	if _, err := joincode.ParseRequest(request); err != nil {
		return localapi.AdmitResult{}, &localapi.OpError{HTTPStatus: http.StatusBadRequest, Code: "invalid_request", Message: err.Error()}
	}
	opID, err := ids.NewUUID()
	if err != nil {
		return localapi.AdmitResult{}, err
	}
	if s.getStore().IsLeader() {
		return s.admitAsLeader(request, actor, s.cfg.NodeID, opID)
	}
	var res localapi.AdmitResult
	err = s.toLeader(ctx, raftnet.OpAdmit, forwardAdmit{Request: request, Actor: actor, OpID: opID}, &res)
	return res, err
}

// Withdraw implements localapi.Backend: "skep-cluster admit --withdraw".
func (s *Service) Withdraw(ctx context.Context, name, actor string) (localapi.WithdrawResult, error) {
	if !clusterstate.ValidName(name) {
		return localapi.WithdrawResult{}, &localapi.OpError{HTTPStatus: http.StatusBadRequest, Code: "invalid_request", Message: "not a valid node name"}
	}
	if s.joinState() != nil {
		return localapi.WithdrawResult{}, &localapi.OpError{HTTPStatus: http.StatusConflict, Code: "joining",
			Message: "this node is joining a cluster; withdraw on a member"}
	}
	opID, err := ids.NewUUID()
	if err != nil {
		return localapi.WithdrawResult{}, err
	}
	if s.getStore().IsLeader() {
		return s.withdrawAsLeader(name, actor, s.cfg.NodeID, opID)
	}
	var res localapi.WithdrawResult
	err = s.toLeader(ctx, raftnet.OpWithdraw, forwardWithdraw{Name: name, Actor: actor, OpID: opID}, &res)
	return res, err
}

// toLeader passes an operation to the leader as a typed Skep RPC.
func (s *Service) toLeader(ctx context.Context, op string, in, out any) error {
	id, _ := s.getStore().Leader()
	if id == "" {
		return &localapi.OpError{HTTPStatus: http.StatusServiceUnavailable, Code: "no_leader",
			Message: "the cluster has no leader now (is a majority of the voters running?); nothing was written"}
	}
	leader, ok := s.machine.Member(id)
	if !ok || leader.Address == "" {
		return &localapi.OpError{HTTPStatus: http.StatusServiceUnavailable, Code: "no_leader",
			Message: "the leader " + id + " is not a member this node knows; nothing was written"}
	}
	err := s.caller.Call(ctx, leader.Address, leader.Pin, op, in, out)
	var pe *raftnet.PeerError
	var ou *raftnet.OutcomeUnknownError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ou):
		return &localapi.OpError{HTTPStatus: http.StatusServiceUnavailable, Code: "outcome_unknown",
			Message: "sent to the leader " + leader.Name + " and no answer came; check \"skep-cluster status\": " + err.Error()}
	case errors.As(err, &pe):
		status := http.StatusConflict
		switch pe.Code {
		case raftnet.CodeNotLeader, raftnet.CodeUnavailable:
			status = http.StatusServiceUnavailable
		case raftnet.CodeOutcomeUnknown, "not_committed":
			status = http.StatusServiceUnavailable
		case raftnet.CodeInvalidRequest:
			status = http.StatusBadRequest
		}
		return &localapi.OpError{HTTPStatus: status, Code: pe.Code, Message: "the leader " + leader.Name + " answered: " + pe.Message}
	}
	return &localapi.OpError{HTTPStatus: http.StatusServiceUnavailable, Code: "no_leader",
		Message: "the leader " + leader.Name + " could not be reached; nothing was sent: " + err.Error()}
}

// applyError maps a write's error for the local API.
func applyError(err error) error {
	switch {
	case errors.Is(err, clusterstate.ErrRefused):
		return &localapi.OpError{HTTPStatus: http.StatusConflict, Code: "refused", Message: err.Error()}
	case errors.Is(err, store.ErrNotLeader):
		return &localapi.OpError{HTTPStatus: http.StatusServiceUnavailable, Code: "not_committed",
			Message: "not committed: this node lost the leadership before the entry was written (" + err.Error() + ")"}
	case errors.Is(err, store.ErrOutcomeUnknown):
		return &localapi.OpError{HTTPStatus: http.StatusServiceUnavailable, Code: "not_committed",
			Message: "not committed within the time limit; the outcome is unknown (is a majority of the voters running?). " +
				"\"skep-cluster status\" lists the pending entries once the cluster has a leader: " + err.Error()}
	}
	return err
}

func (s *Service) admitAsLeader(request, actor, origin, opID string) (localapi.AdmitResult, error) {
	req, err := joincode.ParseRequest(request)
	if err == nil {
		err = joincode.CheckPorts(req.Node)
	}
	if err != nil {
		return localapi.AdmitResult{}, &localapi.OpError{HTTPStatus: http.StatusBadRequest, Code: "invalid_request", Message: err.Error()}
	}
	expires := s.now().Add(PendingLifetime).Unix()
	e, err := clusterstate.NewEntry(clusterstate.TypePendingCreate, opID, actor, origin, clusterstate.PendingCreate{
		NodeID: req.NodeID, Name: req.Name, Address: req.Address, Pin: req.Pin, ExpiresAt: expires})
	if err != nil {
		return localapi.AdmitResult{}, &localapi.OpError{HTTPStatus: http.StatusBadRequest, Code: "invalid_request", Message: err.Error()}
	}
	if _, err := s.getStore().Apply(e); err != nil {
		return localapi.AdmitResult{}, applyError(err)
	}
	view := s.machine.View()
	adm := joincode.Admission{ClusterID: view.ClusterID, For: req.Node, ExpiresAt: expires}
	for _, m := range view.Members {
		adm.Members = append(adm.Members, joincode.Node{NodeID: m.ID, Name: m.Name, Address: m.Address, Pin: m.Pin})
	}
	line, err := joincode.EncodeAdmission(adm)
	if err != nil {
		return localapi.AdmitResult{}, err
	}
	return localapi.AdmitResult{Admission: line, ClusterID: view.ClusterID, NodeID: req.NodeID, Name: req.Name, Pin: req.Pin,
		ExpiresAt: expires, OpID: opID}, nil
}

func (s *Service) withdrawAsLeader(name, actor, origin, opID string) (localapi.WithdrawResult, error) {
	var id string
	for _, p := range s.machine.View().Pendings {
		if p.Name == name {
			id = p.NodeID
		}
	}
	if id == "" {
		return localapi.WithdrawResult{}, &localapi.OpError{HTTPStatus: http.StatusNotFound, Code: "not_found",
			Message: "there is no pending entry named " + name}
	}
	e, err := clusterstate.NewEntry(clusterstate.TypePendingWithdraw, opID, actor, origin, clusterstate.PendingWithdraw{NodeID: id, Name: name})
	if err != nil {
		return localapi.WithdrawResult{}, err
	}
	if _, err := s.getStore().Apply(e); err != nil {
		return localapi.WithdrawResult{}, applyError(err)
	}
	return localapi.WithdrawResult{NodeID: id, Name: name, OpID: opID}, nil
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
