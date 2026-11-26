package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/guestapi"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
	"github.com/greenbeanorg/skep/internal/cluster/store"
)

// GuestsRead implements localapi.Backend (step 8d-1, D22): the guest
// inventory with each guest's computed flag and its owner's node name, and
// the facts users-read gives. A joining or removed node answers no guests:
// what its state machine holds is the log's history, not the cluster's
// inventory.
func (s *Service) GuestsRead(ctx context.Context) (guestapi.GuestsAnswer, error) {
	a := guestapi.GuestsAnswer{Guests: []guestapi.GuestInfo{}, Reservations: []guestapi.ReservationInfo{}}
	if r := s.removedRecord(); r != nil {
		a.ClusterID, a.Removed = r.ClusterID, true
		return a, nil
	}
	st := s.getStore()
	if st == nil {
		a.Removed = true
		return a, nil
	}
	_, stalled := s.machine.AppliedIndex()
	a.LeaderKnown, a.Stalled = st.Stats().LeaderID != "", stalled
	if js := s.joinState(); js != nil {
		a.Joining, a.ClusterID = true, js.Admission.ClusterID
		return a, nil
	}
	g := s.machine.Guests()
	names := map[string]string{}
	for _, m := range s.machine.View().Members {
		names[m.ID] = m.Name
	}
	a.ClusterID, a.GuestsIndex = g.ClusterID, g.Index
	for _, x := range g.Guests {
		a.Guests = append(a.Guests, guestapi.GuestInfo{Guest: x, OwnerName: names[x.Owner]})
	}
	for _, x := range g.Reservations {
		a.Reservations = append(a.Reservations, guestapi.ReservationInfo{Reservation: x, OwnerName: names[x.Owner]})
	}
	return a, nil
}

// GuestsWrite implements localapi.Backend (D23): one guest entry whose
// owner is this node. On the leader it is built here; on a member it is
// passed to the leader, which sets the owner from this node's key.
func (s *Service) GuestsWrite(ctx context.Context, req guestapi.WriteRequest) (guestapi.WriteResult, error) {
	if s.isRemoved() {
		return guestapi.WriteResult{}, s.removedError()
	}
	if s.joinState() != nil {
		return guestapi.WriteResult{}, &localapi.OpError{HTTPStatus: http.StatusServiceUnavailable, Code: "no_leader",
			Message: "this node is joining a cluster; it owns no guests yet; nothing was written"}
	}
	if err := checkGuestsWrite(req); err != nil {
		return guestapi.WriteResult{}, err
	}
	if s.getStore().IsLeader() {
		return s.guestsWriteAsLeader(req, s.cfg.NodeID)
	}
	var res guestapi.WriteResult
	err := s.toLeader(ctx, raftnet.OpGuestsWrite, req, &res)
	return res, err
}

func checkGuestsWrite(req guestapi.WriteRequest) error {
	if !clusterstate.IsGuestType(req.Type) || !opIDRE.MatchString(req.OpID) || !guestapi.ValidActor(req.Actor) ||
		len(req.RequestID) > 128 || len(req.Data) == 0 {
		return &localapi.OpError{HTTPStatus: http.StatusBadRequest, Code: raftnet.CodeInvalidRequest,
			Message: "the guest write needs a guest entry type, an operation ID, an actor (user:NAME or reconciliation) and data"}
	}
	return nil
}

// handleGuestsWrite is the leader's side of a passed guests-write. It
// decodes, checks the term's barrier, and only then computes anything
// (as handleUsersWrite). The owner is the node of the key that called.
func (s *Service) handleGuestsWrite(from raftnet.Peer, body json.RawMessage) (any, error) {
	var req guestapi.WriteRequest
	if err := strictJSON(body, &req); err != nil {
		return nil, &raftnet.PeerError{Code: raftnet.CodeInvalidRequest, Message: "the guests-write request is not valid"}
	}
	if hint, ok := s.notLeader(); ok {
		return nil, hint
	}
	if err := checkGuestsWrite(req); err != nil {
		return nil, toPeerError(err)
	}
	res, err := s.guestsWriteAsLeader(req, from.NodeID)
	return res, toPeerError(err)
}

// guestWriteStatus is the HTTP status of a refusal code (D18).
var guestWriteStatus = map[string]int{
	clusterstate.CodeNameTaken:      http.StatusConflict,
	clusterstate.CodeTooManyGuests:  http.StatusConflict,
	clusterstate.CodeOwnerNotMember: http.StatusConflict,
	clusterstate.CodeNotOwner:       http.StatusConflict,
	clusterstate.CodeInvalid:        http.StatusBadRequest,
}

// guestsWriteAsLeader builds and commits the entry. owner is this node,
// or the node of the key that passed the write on; it is both the
// entry's origin and its owner.
func (s *Service) guestsWriteAsLeader(req guestapi.WriteRequest, owner string) (guestapi.WriteResult, error) {
	data, err := guestapi.Entry(req.Type, req.Data, owner, s.now().Unix())
	if err != nil {
		return guestapi.WriteResult{}, &localapi.OpError{HTTPStatus: http.StatusBadRequest, Code: raftnet.CodeInvalidRequest,
			Message: "the data is not a " + req.Type + " entry's (no owner and no time may be given)"}
	}
	e, err := clusterstate.NewEntry(req.Type, req.OpID, req.Actor, owner, data)
	if err != nil {
		return guestapi.WriteResult{}, &localapi.OpError{HTTPStatus: http.StatusBadRequest, Code: raftnet.CodeInvalidRequest, Message: err.Error()}
	}
	e.RequestID = req.RequestID
	res, err := s.getStore().Apply(e)
	if err != nil {
		if code := clusterstate.RefusalCode(err); code != "" && errors.Is(err, clusterstate.ErrRefused) {
			return guestapi.WriteResult{}, &localapi.OpError{HTTPStatus: guestWriteStatus[code], Code: code, Message: err.Error()}
		}
		if errors.Is(err, store.ErrOutcomeUnknown) {
			return guestapi.WriteResult{}, &localapi.OpError{HTTPStatus: http.StatusServiceUnavailable, Code: "outcome_unknown",
				Message: "the entry may or may not have been committed; repeat it with the same operation ID: " + err.Error()}
		}
		return guestapi.WriteResult{}, applyError(err)
	}
	out := guestapi.WriteResult{Index: res.Index, ClusterID: s.machine.View().ClusterID, Outcome: guestapi.WriteApplied}
	switch {
	case res.Duplicate:
		out.Index, out.Outcome = res.OpIndex, guestapi.WriteDuplicate
	case res.Unchanged:
		out.Outcome = guestapi.WriteUnchanged
	}
	return out, nil
}
