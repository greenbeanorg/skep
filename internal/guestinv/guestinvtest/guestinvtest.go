// Package guestinvtest is a test double of skep-cluster's guest socket: a
// real clusterstate.Machine behind guests-read and guests-write, with the
// owner and time set as the service sets them. It is not used by any
// program.
package guestinvtest

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/guestapi"
)

// Node IDs of the test cluster.
const (
	ClusterID = "c0000000-0000-4000-8000-0000000000aa"
	NodeA     = "c0000000-0000-4000-8000-0000000000a1"
	NodeB     = "c0000000-0000-4000-8000-0000000000b1"
)

// Cluster is a two-member cluster's state machine (smith = NodeA,
// langrishe = NodeB). Node returns the socket of one member.
type Cluster struct {
	mu   sync.Mutex
	m    *clusterstate.Machine
	next uint64
	// Now is the leader's clock in Unix seconds.
	Now int64
}

// New returns the cluster with both members in state member.
func New() *Cluster {
	c := &Cluster{m: clusterstate.New(), next: 1, Now: 1_900_000_000}
	pin := func(b string) string { return "sha256:" + strings.Repeat(b, 64) }
	must := func(r clusterstate.Result) {
		if r.Err != nil {
			panic(r.Err)
		}
	}
	must(c.apply(clusterstate.TypeClusterInit, "i-1", "system", NodeA, clusterstate.ClusterInit{ClusterID: ClusterID,
		Member: clusterstate.Member{ID: NodeA, Name: "smith", Address: "10.0.0.1:8444", Pin: pin("a"), State: clusterstate.StateMember}}))
	must(c.apply(clusterstate.TypePendingCreate, "i-2", "system", NodeA, clusterstate.PendingCreate{NodeID: NodeB, Name: "langrishe",
		Address: "10.0.0.2:8444", Pin: pin("b"), ExpiresAt: 2_000_000_000}))
	must(c.apply(clusterstate.TypeMemberJoin, "i-3", "system", NodeA, clusterstate.MemberJoin{NodeID: NodeB, Pin: pin("b"), At: 1_800_000_000}))
	must(c.apply(clusterstate.TypeMemberPromoted, "i-4", "system", NodeA, clusterstate.MemberPromoted{NodeID: NodeB}))
	return c
}

func (c *Cluster) apply(typ, op, actor, origin string, data any) clusterstate.Result {
	e, err := clusterstate.NewEntry(typ, op, actor, origin, data)
	if err != nil {
		return clusterstate.Result{Err: err}
	}
	raw, err := e.Encode()
	if err != nil {
		return clusterstate.Result{Err: err}
	}
	r := c.m.Apply(c.next, raw)
	c.next++
	return r
}

// Machine is the state machine, for checks.
func (c *Cluster) Machine() *clusterstate.Machine { return c.m }

// Socket is one member's guest socket. Down makes every call fail as an
// unreachable socket; Unanswered as a request without an answer.
type Socket struct {
	c          *Cluster
	node       string
	Down       bool
	Unanswered bool
	// Writes counts the guests-write calls that reached the machine.
	Writes int
	// WriteErr, when set, is every guests-write's answer (reads still
	// work); Calls counts the guests-write calls, answered or not.
	WriteErr error
	Calls    int
}

// Node returns the socket of a member.
func (c *Cluster) Node(id string) *Socket { return &Socket{c: c, node: id} }

// Read implements guestinv.Client.
func (s *Socket) Read(context.Context) (guestapi.GuestsAnswer, error) {
	if s.Down {
		return guestapi.GuestsAnswer{}, guestapi.ErrUnreachable
	}
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	g := s.c.m.Guests()
	names := map[string]string{}
	for _, m := range s.c.m.View().Members {
		names[m.ID] = m.Name
	}
	a := guestapi.GuestsAnswer{ClusterID: g.ClusterID, GuestsIndex: g.Index, LeaderKnown: true,
		Guests: []guestapi.GuestInfo{}, Reservations: []guestapi.ReservationInfo{}}
	for _, x := range g.Guests {
		a.Guests = append(a.Guests, guestapi.GuestInfo{Guest: x, OwnerName: names[x.Owner]})
	}
	for _, x := range g.Reservations {
		a.Reservations = append(a.Reservations, guestapi.ReservationInfo{Reservation: x, OwnerName: names[x.Owner]})
	}
	return a, nil
}

// Write implements guestinv.Client: the owner is this socket's node, as
// skep-cluster sets it.
func (s *Socket) Write(_ context.Context, req guestapi.WriteRequest) (guestapi.WriteResult, error) {
	s.Calls++
	switch {
	case s.WriteErr != nil:
		return guestapi.WriteResult{}, s.WriteErr
	case s.Down:
		return guestapi.WriteResult{}, guestapi.ErrUnreachable
	case s.Unanswered:
		return guestapi.WriteResult{}, guestapi.ErrNoAnswer
	}
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	if !clusterstate.IsGuestType(req.Type) || !guestapi.ValidActor(req.Actor) {
		return guestapi.WriteResult{}, &guestapi.Error{HTTPStatus: http.StatusBadRequest, Code: "invalid_request", Message: "not a guest write"}
	}
	data, err := guestapi.Entry(req.Type, req.Data, s.node, s.c.Now)
	if err != nil {
		return guestapi.WriteResult{}, &guestapi.Error{HTTPStatus: http.StatusBadRequest, Code: "invalid_request", Message: err.Error()}
	}
	s.Writes++
	r := s.c.apply(req.Type, req.OpID, req.Actor, s.node, data)
	if r.Err != nil {
		if code := clusterstate.RefusalCode(r.Err); code != "" && errors.Is(r.Err, clusterstate.ErrRefused) {
			return guestapi.WriteResult{}, &guestapi.Error{HTTPStatus: http.StatusConflict, Code: code, Message: r.Err.Error()}
		}
		return guestapi.WriteResult{}, &guestapi.Error{HTTPStatus: http.StatusInternalServerError, Code: "internal_error", Message: r.Err.Error()}
	}
	out := guestapi.WriteResult{Index: r.Index, ClusterID: ClusterID, Outcome: guestapi.WriteApplied}
	switch {
	case r.Duplicate:
		out.Index, out.Outcome = r.OpIndex, guestapi.WriteDuplicate
	case r.Unchanged:
		out.Outcome = guestapi.WriteUnchanged
	}
	return out, nil
}
