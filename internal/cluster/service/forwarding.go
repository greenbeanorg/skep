package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/peerapi"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
	"github.com/greenbeanorg/skep/internal/cluster/store"
)

// SupportsRequest is the body of the supports peer operation (step 8e-1,
// D7): an entry type and a version.
type SupportsRequest struct {
	Type    string `json:"type"`
	Version int    `json:"version"`
}

// SupportsAnswer is its answer.
type SupportsAnswer struct {
	Supported bool `json:"supported"`
}

// supportsTimeout bounds each supports call of the api-key guard (D8).
const supportsTimeout = 5 * time.Second

// handleSupports answers whether this build applies the type and version.
func handleSupports(body json.RawMessage) (any, error) {
	var req SupportsRequest
	if err := strictJSON(body, &req); err != nil {
		return nil, &raftnet.PeerError{Code: raftnet.CodeInvalidRequest, Message: "the supports request is not valid"}
	}
	max, ok := clusterstate.Supported[req.Type]
	return SupportsAnswer{Supported: ok && req.Version >= 1 && req.Version <= max}, nil
}

// checkAPIKey checks an api-key request (D9): a key pin and an operation
// ID that is a lower-case UUID.
func checkAPIKey(req peerapi.APIKeyRequest) error {
	if !clusterstate.ValidPin(req.Pin) || !clusterstate.ValidNodeID(req.OpID) {
		return &localapi.OpError{HTTPStatus: http.StatusBadRequest, Code: raftnet.CodeInvalidRequest,
			Message: "api-key needs a key pin (sha256:...) and an operation ID that is a lower-case UUID; nothing was written"}
	}
	return nil
}

// APIKey implements localapi.Backend (D9): this node's API pin. The node
// is always this node. The leader handles it itself; a member passes it on
// as the peer operation api-key.
func (s *Service) APIKey(ctx context.Context, req peerapi.APIKeyRequest) (peerapi.APIKeyResult, error) {
	if s.isRemoved() {
		return peerapi.APIKeyResult{}, s.removedError()
	}
	if s.joinState() != nil {
		return peerapi.APIKeyResult{}, &localapi.OpError{HTTPStatus: http.StatusServiceUnavailable, Code: "joining",
			Message: "this node is joining a cluster; it registers its API key once it is a member; nothing was written"}
	}
	if s.cfg.ListenAddress == "" {
		return peerapi.APIKeyResult{}, &localapi.OpError{HTTPStatus: http.StatusConflict, Code: "no_listen_address",
			Message: "this node has no listen address: a single host forwards nothing and registers no API key; nothing was written"}
	}
	if err := checkAPIKey(req); err != nil {
		return peerapi.APIKeyResult{}, err
	}
	if s.getStore().IsLeader() {
		if hint, ok := s.notLeader(); ok {
			return peerapi.APIKeyResult{}, &localapi.OpError{HTTPStatus: http.StatusServiceUnavailable, Code: hint.Code, Message: hint.Message}
		}
		return s.apiKeyAsLeader(ctx, req, s.cfg.NodeID)
	}
	var res peerapi.APIKeyResult
	err := s.toLeader(ctx, raftnet.OpAPIKey, req, &res)
	return res, err
}

// handleAPIKey is the leader's side of a passed api-key (D8): decode
// strictly, check the term's barrier before anything is computed, and take
// the node from the calling key.
func (s *Service) handleAPIKey(ctx context.Context, from raftnet.Peer, body json.RawMessage) (any, error) {
	var req peerapi.APIKeyRequest
	if err := strictJSON(body, &req); err != nil {
		return nil, &raftnet.PeerError{Code: raftnet.CodeInvalidRequest, Message: "the api-key request is not valid"}
	}
	if hint, ok := s.notLeader(); ok {
		return nil, hint
	}
	if err := checkAPIKey(req); err != nil {
		return nil, toPeerError(err)
	}
	res, err := s.apiKeyAsLeader(ctx, req, from.NodeID)
	return res, toPeerError(err)
}

// apiKeyGuard is D8's guard: every other member entry, whatever its state,
// must answer supports for member.api_key version 1 with true, in this
// call. Registration is automatic, and a build before step 8e-1 stops its
// state machine at this entry type. It returns "" when every one did, or
// the first member (by ID) that did not, with why.
func (s *Service) apiKeyGuard(ctx context.Context) string {
	var others []clusterstate.Member
	for _, m := range s.machine.View().Members {
		if m.ID != s.cfg.NodeID {
			others = append(others, m)
		}
	}
	why := make([]string, len(others))
	var wg sync.WaitGroup
	for i, m := range others {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, supportsTimeout)
			defer cancel()
			var a SupportsAnswer
			err := s.callMember(cctx, m, raftnet.OpSupports, SupportsRequest{Type: clusterstate.TypeMemberAPIKey, Version: 1}, &a)
			var pe *raftnet.PeerError
			switch {
			case err == nil && a.Supported:
			case err == nil:
				why[i] = "its build does not apply member.api_key version 1"
			case errors.As(err, &pe) && pe.Code == raftnet.CodeForbidden:
				why[i] = "its build does not know the operation (an earlier build): upgrade it first"
			default:
				why[i] = "it did not answer: " + err.Error()
			}
		}()
	}
	wg.Wait()
	for i, m := range others {
		if why[i] != "" {
			return m.Name + " (" + m.ID + "): " + why[i]
		}
	}
	return ""
}

// apiKeyStatus is the HTTP status of a member.api_key refusal code.
var apiKeyStatus = map[string]int{
	clusterstate.CodeNotFound: http.StatusNotFound,
	clusterstate.CodeInvalid:  http.StatusConflict,
}

// apiKeyAsLeader writes the entry for node, after the guard. Its
// operation ID is apikey:<node>:<op_id>, so that no node's request can
// make another node's a duplicate.
func (s *Service) apiKeyAsLeader(ctx context.Context, req peerapi.APIKeyRequest, node string) (peerapi.APIKeyResult, error) {
	if who := s.apiKeyGuard(ctx); who != "" {
		return peerapi.APIKeyResult{}, &localapi.OpError{HTTPStatus: http.StatusServiceUnavailable, Code: raftnet.CodeUnavailable,
			Message: "not every member can apply member.api_key: " + who + "; nothing was written"}
	}
	e, err := clusterstate.NewEntry(clusterstate.TypeMemberAPIKey, "apikey:"+node+":"+req.OpID, "node:"+node, node,
		clusterstate.MemberAPIKey{NodeID: node, APIPin: req.Pin, At: s.now().Unix()})
	if err != nil {
		return peerapi.APIKeyResult{}, &localapi.OpError{HTTPStatus: http.StatusBadRequest, Code: raftnet.CodeInvalidRequest, Message: err.Error()}
	}
	res, err := s.getStore().Apply(e)
	if err != nil {
		if code := clusterstate.RefusalCode(err); code != "" && errors.Is(err, clusterstate.ErrRefused) {
			return peerapi.APIKeyResult{}, &localapi.OpError{HTTPStatus: apiKeyStatus[code], Code: code, Message: err.Error()}
		}
		if errors.Is(err, store.ErrOutcomeUnknown) {
			return peerapi.APIKeyResult{}, &localapi.OpError{HTTPStatus: http.StatusServiceUnavailable, Code: "outcome_unknown",
				Message: "the entry may or may not have been committed; repeat it with the same operation ID: " + err.Error()}
		}
		return peerapi.APIKeyResult{}, applyError(err)
	}
	out := peerapi.APIKeyResult{Index: res.Index, ClusterID: s.machine.View().ClusterID, Outcome: peerapi.APIKeyApplied}
	switch {
	case res.Duplicate:
		out.Index, out.Outcome = res.OpIndex, peerapi.APIKeyDuplicate
	case res.Unchanged:
		out.Outcome = peerapi.APIKeyUnchanged
	}
	return out, nil
}

// onMembership wakes the peers-read long polls; OnApplied calls it.
func (s *Service) onMembership() {
	s.peersMu.Lock()
	if s.peersCh != nil {
		close(s.peersCh)
		s.peersCh = nil
	}
	s.peersMu.Unlock()
}

func (s *Service) membershipChanged() <-chan struct{} {
	s.peersMu.Lock()
	defer s.peersMu.Unlock()
	if s.peersCh == nil {
		s.peersCh = make(chan struct{})
	}
	return s.peersCh
}

// upToDate is the status's up_to_date: the machine is not stopped, the
// library has applied the commit index, and everything between the
// machine's index and the library's is the library's own.
func upToDate(st Store, view clusterstate.View) bool {
	stats := st.Stats()
	return view.Stalled == nil && stats.AppliedIndex >= stats.CommitIndex && st.LibraryEntriesOnly(view.AppliedIndex, stats.AppliedIndex)
}

// PeersRead implements localapi.Backend (D11): it waits up to wait for the
// membership index to differ from after, then answers. A node that is
// joining, removed, stalled or not current answers no peers; a node
// without a leader answers the peers its state machine holds.
func (s *Service) PeersRead(ctx context.Context, after uint64, wait time.Duration) (peerapi.PeersAnswer, error) {
	a := peerapi.PeersAnswer{NodeID: s.cfg.NodeID, NodeName: s.cfg.NodeName, ListenAddress: s.cfg.ListenAddress, Peers: []peerapi.Peer{}}
	if r := s.removedRecord(); r != nil {
		a.ClusterID, a.Removed = r.ClusterID, true
		return a, nil
	}
	if wait > peerapi.MaxPeersWait {
		wait = peerapi.MaxPeersWait
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for s.joinState() == nil && s.machine.View().MembershipIndex == after {
		ch := s.membershipChanged()
		if s.machine.View().MembershipIndex != after {
			break
		}
		select {
		case <-ch:
			continue
		case <-deadline.C:
		case <-ctx.Done():
		}
		break
	}
	st := s.getStore()
	if s.removedRecord() != nil || st == nil {
		a.Removed = true
		return a, nil
	}
	view := s.machine.View()
	a.MembershipIndex, a.LeaderKnown, a.Stalled = view.MembershipIndex, st.Stats().LeaderID != "", view.Stalled != nil
	if js := s.joinState(); js != nil {
		a.Joining, a.ClusterID = true, js.Admission.ClusterID
		return a, nil
	}
	a.ClusterID = view.ClusterID
	a.Current = s.acceptedNow().source == "state machine" && upToDate(st, view)
	if a.Stalled || !a.Current {
		return a, nil
	}
	servers := map[string]bool{}
	if list, err := st.Servers(); err == nil {
		for _, sv := range list {
			servers[sv.ID] = true
		}
	}
	clocks := map[string]localapi.ClockStatus{}
	for _, c := range s.clockStatus(view.Members) {
		clocks[c.NodeID] = c
	}
	for _, m := range view.Members {
		c := clocks[m.ID]
		a.Peers = append(a.Peers, peerapi.Peer{NodeID: m.ID, Name: m.Name, Address: m.Address, State: m.State, APIPin: m.APIPin,
			RaftServer: servers[m.ID], Clock: peerapi.Clock{Known: c.Known, OffsetMillis: c.OffsetMillis, UncertaintyMillis: c.UncertaintyMillis}})
	}
	return a, nil
}

// GuestOwner implements localapi.Backend (D12): one guest by its ID from
// this node's state machine. A joining or removed node answers found
// false.
func (s *Service) GuestOwner(ctx context.Context, id string) (peerapi.GuestOwnerAnswer, error) {
	var a peerapi.GuestOwnerAnswer
	if r := s.removedRecord(); r != nil {
		a.ClusterID, a.Removed = r.ClusterID, true
		return a, nil
	}
	st := s.getStore()
	if st == nil {
		a.Removed = true
		return a, nil
	}
	view := s.machine.View()
	a.LeaderKnown, a.Stalled = st.Stats().LeaderID != "", view.Stalled != nil
	if js := s.joinState(); js != nil {
		a.Joining, a.ClusterID = true, js.Admission.ClusterID
		return a, nil
	}
	a.ClusterID = view.ClusterID
	g, ok := s.machine.GuestByID(id)
	if !ok {
		return a, nil
	}
	a.Found, a.Kind, a.Owner = true, g.Kind, g.Owner
	if m, ok := s.machine.Member(g.Owner); ok {
		a.OwnerName = m.Name
	}
	return a, nil
}
