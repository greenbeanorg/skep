package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/guestapi"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
)

const guestID1 = "00000000-0000-4000-8000-0000000000d1"

func guestReq(typ, op, data string) guestapi.WriteRequest {
	return guestapi.WriteRequest{Type: typ, OpID: op, Actor: "user:andy", RequestID: "req-1", Data: json.RawMessage(data)}
}

// V1 (service part): the owner of a guest entry is the node of the caller
// — this node on its own socket, the node of the calling key when a member
// passes the write on — and never the body; a body that names an owner is
// refused by strict decoding, on the socket and from a peer.
func TestGuestOwnerIsTheCallersNode(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe")
	ctx := context.Background()
	l, x := h.leader(), follower(h)

	res, err := x.svc.GuestsWrite(ctx, guestReq(clusterstate.TypeGuestReserve, "g-1", `{"kind":"container","name":"web1","key":"k-1"}`))
	if err != nil || res.Outcome != guestapi.WriteApplied || res.Index == 0 {
		t.Fatalf("reserve through the follower: %+v %v", res, err)
	}
	again, err := x.svc.GuestsWrite(ctx, guestReq(clusterstate.TypeGuestReserve, "g-2", `{"kind":"container","name":"web1","key":"k-1"}`))
	if err != nil || again.Outcome != guestapi.WriteUnchanged {
		t.Fatalf("the same reservation again: %+v %v", again, err)
	}
	if _, err := l.svc.GuestsWrite(ctx, guestReq(clusterstate.TypeGuestReserve, "g-3", `{"kind":"container","name":"web1","key":"k-2"}`)); opCode(err) != clusterstate.CodeNameTaken {
		t.Fatalf("the name on the leader: %v", err)
	}
	// A body with an owner is refused, on the socket and from a peer.
	withOwner := `{"kind":"vm","name":"db1","key":"k-3","owner":"` + l.id + `"}`
	if _, err := x.svc.GuestsWrite(ctx, guestReq(clusterstate.TypeGuestReserve, "g-4", withOwner)); opCode(err) != raftnet.CodeInvalidRequest {
		t.Fatalf("an owner in the body on the socket: %v", err)
	}
	body, _ := json.Marshal(guestReq(clusterstate.TypeGuestReserve, "g-5", withOwner))
	if _, err := l.svc.handleGuestsWrite(raftnet.Peer{NodeID: x.id}, body); err == nil {
		t.Fatal("an owner in a forwarded body was accepted")
	}
	// A forwarded write's owner is the calling key's node, whatever the
	// actor says.
	body, _ = json.Marshal(guestReq(clusterstate.TypeGuestAdd, "g-6", `{"id":"`+guestID1+`","kind":"vm","name":"db1","task_id":""}`))
	if _, err := l.svc.handleGuestsWrite(raftnet.Peer{NodeID: x.id}, body); err != nil {
		t.Fatalf("forwarded add: %v", err)
	}
	eventually(t, "the add on the follower", func() bool { return len(x.svc.machine.Guests().Guests) == 1 })
	a, err := x.svc.GuestsRead(ctx)
	if err != nil || len(a.Guests) != 1 || len(a.Reservations) != 1 || !a.LeaderKnown || a.Joining || a.Removed {
		t.Fatalf("read: %+v %v", a, err)
	}
	if a.Guests[0].Owner != x.id || a.Guests[0].OwnerName != x.name || a.Reservations[0].Owner != x.id {
		t.Fatalf("owners: %+v %+v", a.Guests[0], a.Reservations[0])
	}
	// The leader may not remove the follower's guest as itself.
	if _, err := l.svc.GuestsWrite(ctx, guestReq(clusterstate.TypeGuestRemove, "g-7", `{"id":"`+guestID1+`"}`)); opCode(err) != clusterstate.CodeNotOwner {
		t.Fatalf("remove another node's guest: %v", err)
	}
	// Only the guest types and a valid actor are accepted.
	bad := guestReq(clusterstate.TypeUserDelete, "g-8", `{"id":"`+guestID1+`"}`)
	if _, err := x.svc.GuestsWrite(ctx, bad); opCode(err) != raftnet.CodeInvalidRequest {
		t.Fatalf("a user type: %v", err)
	}
	bad = guestReq(clusterstate.TypeGuestRemove, "g-9", `{"id":"`+guestID1+`"}`)
	bad.Actor = "root"
	if _, err := x.svc.GuestsWrite(ctx, bad); opCode(err) != raftnet.CodeInvalidRequest {
		t.Fatalf("actor root: %v", err)
	}
}

// D17: removing a member that owns a guest is refused before the status
// round and any hand-over: the leader stays, the term does not change.
func TestRemovalRefusedWhileOwningGuests(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	ctx := context.Background()
	l := h.leader()
	if _, err := l.svc.GuestsWrite(ctx, guestReq(clusterstate.TypeGuestReserve, "g-1", `{"kind":"container","name":"web1","key":"k-1"}`)); err != nil {
		t.Fatal(err)
	}
	term := l.st.Stats().Term
	x := follower(h)
	_, err := x.svc.Remove(ctx, localapi.RemoveRequest{Name: l.name, ClusterID: l.svc.machine.View().ClusterID}, "root")
	if opCode(err) != "refused" || h.leader() != l || l.st.Stats().Term != term {
		t.Fatalf("removal of the leader that owns a reservation: %v (leader %v, term %d→%d)", err, h.leader() == l, term, l.st.Stats().Term)
	}
	for _, m := range l.svc.machine.View().Members {
		if m.State != clusterstate.StateMember {
			t.Fatalf("member %s is %s", m.Name, m.State)
		}
	}
}
