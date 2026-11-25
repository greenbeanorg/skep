package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
	"github.com/greenbeanorg/skep/internal/cluster/store"
	"github.com/greenbeanorg/skep/internal/cluster/userapi"
)

// actorRE is the actor of a user entry: an API user or a command on the
// host (D2 of step 8c-1).
var actorRE = regexp.MustCompile(`^(user:[a-z][a-z0-9._-]{0,31}|local:[0-9]{1,10})$`)

// OnUsersChanged is installed as the state machine's users hook: it wakes
// the users-read long polls.
func (s *Service) OnUsersChanged() {
	s.usersMu.Lock()
	if s.usersCh != nil {
		close(s.usersCh)
		s.usersCh = nil
	}
	s.usersMu.Unlock()
}

func (s *Service) usersChanged() <-chan struct{} {
	s.usersMu.Lock()
	defer s.usersMu.Unlock()
	if s.usersCh == nil {
		s.usersCh = make(chan struct{})
	}
	return s.usersCh
}

// UsersRead implements localapi.Backend (D11): it waits up to wait for
// the users index to differ from after, then answers the users. A joining
// or removed node answers no users: what its state machine holds is the
// log's history, not the cluster's users.
func (s *Service) UsersRead(ctx context.Context, after uint64, wait time.Duration) (localapi.UsersAnswer, error) {
	if r := s.removedRecord(); r != nil {
		return localapi.UsersAnswer{ClusterID: r.ClusterID, Users: []clusterstate.User{}, Removed: true}, nil
	}
	if wait > userapi.MaxUsersWait {
		wait = userapi.MaxUsersWait
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for s.joinState() == nil && s.machine.UsersIndex() == after {
		ch := s.usersChanged()
		if s.machine.UsersIndex() != after {
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
		return localapi.UsersAnswer{Users: []clusterstate.User{}, Removed: true}, nil
	}
	stats := st.Stats()
	_, stalled := s.machine.AppliedIndex()
	a := localapi.UsersAnswer{LeaderKnown: stats.LeaderID != "", Stalled: stalled, Users: []clusterstate.User{}}
	if js := s.joinState(); js != nil {
		a.Joining, a.ClusterID = true, js.Admission.ClusterID
		return a, nil
	}
	v := s.machine.Users()
	a.ClusterID, a.UsersIndex = v.ClusterID, v.Index
	if v.Users != nil {
		a.Users = v.Users
	}
	return a, nil
}

// UsersWrite implements localapi.Backend (D11): one user entry, built here
// (origin this node) or by the leader it is passed to.
func (s *Service) UsersWrite(ctx context.Context, req localapi.UsersWriteRequest) (localapi.UsersWriteResult, error) {
	if s.isRemoved() {
		return localapi.UsersWriteResult{}, s.removedError()
	}
	if s.joinState() != nil {
		return localapi.UsersWriteResult{}, &localapi.OpError{HTTPStatus: http.StatusServiceUnavailable, Code: "no_leader",
			Message: "this node is joining a cluster; its users are not the cluster's yet; nothing was written"}
	}
	if err := checkUsersWrite(req); err != nil {
		return localapi.UsersWriteResult{}, err
	}
	if s.getStore().IsLeader() {
		return s.usersWriteAsLeader(req, s.cfg.NodeID)
	}
	var res localapi.UsersWriteResult
	err := s.toLeader(ctx, raftnet.OpUsersWrite, req, &res)
	return res, err
}

func checkUsersWrite(req localapi.UsersWriteRequest) error {
	if !clusterstate.IsUserType(req.Type) || !opIDRE.MatchString(req.OpID) || !actorRE.MatchString(req.Actor) ||
		len(req.RequestID) > 128 || len(req.Data) == 0 {
		return &localapi.OpError{HTTPStatus: http.StatusBadRequest, Code: raftnet.CodeInvalidRequest,
			Message: "the user write needs a user entry type, an operation ID, an actor and data"}
	}
	return nil
}

// handleUsersWrite is the leader's side of a passed users-write. It
// computes nothing before notLeader has checked the term's barrier (V2 of
// the 8b-2b review): every refusal comes from the state machine.
func (s *Service) handleUsersWrite(from raftnet.Peer, body json.RawMessage) (any, error) {
	var req localapi.UsersWriteRequest
	if err := strictJSON(body, &req); err != nil {
		return nil, &raftnet.PeerError{Code: raftnet.CodeInvalidRequest, Message: "the users-write request is not valid"}
	}
	if hint, ok := s.notLeader(); ok {
		return nil, hint
	}
	if err := checkUsersWrite(req); err != nil {
		return nil, toPeerError(err)
	}
	res, err := s.usersWriteAsLeader(req, from.NodeID)
	return res, toPeerError(err)
}

// withTime decodes a user entry's data strictly and sets its time to the
// leader's clock (D2), where the type has one.
func withTime(typ string, raw json.RawMessage, now int64) (any, error) {
	switch typ {
	case clusterstate.TypeUserCreate:
		var d clusterstate.UserCreate
		err := strictJSON(raw, &d)
		d.At = now
		return d, err
	case clusterstate.TypeUserUpdate:
		var d clusterstate.UserUpdate
		err := strictJSON(raw, &d)
		d.At = now
		return d, err
	case clusterstate.TypeUserPassword:
		var d clusterstate.UserPassword
		err := strictJSON(raw, &d)
		d.At = now
		return d, err
	case clusterstate.TypeUserRehash:
		var d clusterstate.UserRehash
		return d, strictJSON(raw, &d)
	case clusterstate.TypeUserDelete:
		var d clusterstate.UserDelete
		return d, strictJSON(raw, &d)
	case clusterstate.TypeUsersImport:
		var d clusterstate.UsersImport
		err := strictJSON(raw, &d)
		d.At = now
		return d, err
	}
	return nil, errors.New("not a user entry type")
}

// userWriteStatus is the HTTP status of a refusal code (D5).
var userWriteStatus = map[string]int{
	clusterstate.CodeUsernameTaken: http.StatusConflict,
	clusterstate.CodeLastAdmin:     http.StatusConflict,
	clusterstate.CodeUnknownRole:   http.StatusBadRequest,
	clusterstate.CodeNotFound:      http.StatusNotFound,
	clusterstate.CodeUsersExist:    http.StatusConflict,
	clusterstate.CodeHashChanged:   http.StatusConflict,
	clusterstate.CodeTooManyUsers:  http.StatusConflict,
	clusterstate.CodeInvalid:       http.StatusBadRequest,
}

func (s *Service) usersWriteAsLeader(req localapi.UsersWriteRequest, origin string) (localapi.UsersWriteResult, error) {
	data, err := withTime(req.Type, req.Data, s.now().Unix())
	if err != nil {
		return localapi.UsersWriteResult{}, &localapi.OpError{HTTPStatus: http.StatusBadRequest, Code: raftnet.CodeInvalidRequest,
			Message: "the data is not a " + req.Type + " entry's"}
	}
	e, err := clusterstate.NewEntry(req.Type, req.OpID, req.Actor, origin, data)
	if err != nil {
		return localapi.UsersWriteResult{}, &localapi.OpError{HTTPStatus: http.StatusBadRequest, Code: raftnet.CodeInvalidRequest, Message: err.Error()}
	}
	e.RequestID = req.RequestID
	if raw, err := e.Encode(); err != nil || (req.Type == clusterstate.TypeUsersImport && len(raw) > clusterstate.MaxImportBytes) {
		return localapi.UsersWriteResult{}, &localapi.OpError{HTTPStatus: http.StatusBadRequest, Code: clusterstate.CodeInvalid,
			Message: "the encoded import is larger than 192 KiB; nothing was written"}
	}
	res, err := s.getStore().Apply(e)
	if err != nil {
		if code := clusterstate.RefusalCode(err); code != "" && errors.Is(err, clusterstate.ErrRefused) {
			return localapi.UsersWriteResult{}, &localapi.OpError{HTTPStatus: userWriteStatus[code], Code: code, Message: err.Error()}
		}
		if errors.Is(err, store.ErrOutcomeUnknown) {
			return localapi.UsersWriteResult{}, &localapi.OpError{HTTPStatus: http.StatusServiceUnavailable, Code: "outcome_unknown",
				Message: "the entry may or may not have been committed; repeat it with the same operation ID: " + err.Error()}
		}
		return localapi.UsersWriteResult{}, applyError(err)
	}
	out := localapi.UsersWriteResult{Index: res.Index, ClusterID: s.machine.View().ClusterID, Outcome: userapi.WriteApplied}
	if res.Duplicate {
		out.Index, out.Outcome = res.OpIndex, userapi.WriteDuplicate
	}
	if id := userIDOf(data); id != "" {
		if u, ok := s.machine.UserByID(id); ok {
			info := userapi.InfoOf(u)
			out.User = &info
		}
	}
	return out, nil
}

func userIDOf(data any) string {
	switch d := data.(type) {
	case clusterstate.UserCreate:
		return d.ID
	case clusterstate.UserUpdate:
		return d.ID
	case clusterstate.UserPassword:
		return d.ID
	case clusterstate.UserRehash:
		return d.ID
	}
	return ""
}
