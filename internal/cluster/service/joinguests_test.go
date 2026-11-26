package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joincode"
	"github.com/greenbeanorg/skep/internal/cluster/joinstate"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
)

var (
	wuGuest1 = clusterstate.JoinGuest{ID: "d0000000-0000-4000-8000-000000000001", Kind: clusterstate.KindContainer, Name: "j1"}
	wuGuest2 = clusterstate.JoinGuest{ID: "d0000000-0000-4000-8000-000000000002", Kind: clusterstate.KindVM, Name: "v2"}
	wuWeb1   = clusterstate.JoinGuest{ID: "d0000000-0000-4000-8000-000000000003", Kind: clusterstate.KindContainer, Name: "web1"}
)

// ownedBy lists the IDs of the guests n's inventory gives owner.
func ownedBy(n *tnode, owner string) []string {
	var out []string
	for _, g := range n.svc.machine.Guests().Guests {
		if g.Owner == owner {
			out = append(out, g.ID)
		}
	}
	return out
}

func joinWithGuests(h *harness, j *tnode, adm joincode.Admission, guests ...clusterstate.JoinGuest) StageOutcome {
	h.t.Helper()
	h.stop(j)
	out := StageJoin(context.Background(), j.dir, j.cfg, j.key, adm, guests, time.Now(), 100*time.Millisecond)
	h.start(j)
	return out
}

// D3, D16, D27: a host joins with guests across an in-process cluster. A
// name the cluster holds refuses the join with the leader's text, and the
// host's one-node state is back; with free names the guests are in every
// node's inventory, owned by the new member, added by its member.join.
func TestJoinWithGuests(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe")
	l := h.leader()
	ctx := context.Background()
	if _, err := l.svc.GuestsWrite(ctx, guestReq(clusterstate.TypeGuestReserve, "g-1", `{"kind":"container","name":"web1","key":"k-1"}`)); err != nil {
		t.Fatal(err)
	}
	j := h.node("wu")
	h.start(j)
	h.eventually("wu leads its one-node cluster", 20*time.Second, func() bool { return j.svc.machine.View().ClusterID != "" })
	ownCluster := j.svc.machine.View().ClusterID
	adm := h.admit(l, j)

	out := joinWithGuests(h, j, adm, wuGuest1, wuWeb1)
	if out.Outcome != JoinRefused || !strings.Contains(out.Message, `the container name "web1" is reserved by node `+l.name) {
		t.Fatalf("a taken name: %+v", out)
	}
	if js, _ := joinstate.Read(j.dir); js != nil || len(standalones(t, j.dir)) != 0 {
		t.Fatal("join.json or a moved-aside state is left after the refusal")
	}
	h.eventually("wu's one-node cluster is back", 20*time.Second, func() bool {
		return j.st != nil && j.st.IsLeader() && j.svc.machine.View().ClusterID == ownCluster
	})
	if p := pendings(l); len(p) != 1 || p[0].NodeID != j.id {
		t.Fatalf("the pending entry changed: %+v", p)
	}
	if len(ownedBy(l, j.id)) != 0 {
		t.Fatal("a refused join added guests")
	}

	out = joinWithGuests(h, j, adm, wuGuest1, wuGuest2)
	if out.Outcome != JoinAccepted {
		t.Fatalf("join: %+v", out)
	}
	h.joined(j, 60*time.Second)
	for _, n := range h.running() {
		n := n
		h.eventually("wu's guests on "+n.name, 10*time.Second, func() bool { return len(ownedBy(n, j.id)) == 2 })
	}
	mem, _ := l.svc.machine.Member(j.id)
	for _, g := range l.svc.machine.Guests().Guests {
		if g.Owner == j.id && (g.AddedIndex != mem.JoinedIndex || g.Key != "" || g.TaskID != "") {
			t.Fatalf("guest %+v, joined at %d", g, mem.JoinedIndex)
		}
	}
	a, err := j.svc.GuestsRead(ctx)
	if err != nil || a.Joining || len(a.Guests) != 2 || len(a.Reservations) != 1 {
		t.Fatalf("wu's read after the join: %+v %v", a, err)
	}
}

// D14, D27: a join whose outcome was unknown is repeated by the service
// with the same operation ID and the same list from join.json, and the
// cluster adds the guests once.
func TestJoinWithGuestsRepeatAfterUnknown(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe")
	l := h.leader()
	j := h.node("wu")
	h.start(j)
	h.eventually("wu leads its one-node cluster", 20*time.Second, func() bool { return j.svc.machine.View().ClusterID != "" })
	adm := h.admit(l, j)
	// The first call goes to a member that takes it and never answers.
	silent := startSilentMember(t, "127.0.88.21:8444")
	unk := adm
	unk.Members = []joincode.Node{{NodeID: newID(t), Name: "silent", Address: "127.0.88.21:8444", Pin: silent.Pin()}}
	h.stop(j)
	out := StageJoin(context.Background(), j.dir, j.cfg, j.key, unk, []clusterstate.JoinGuest{wuGuest1, wuGuest2}, time.Now(), 50*time.Millisecond)
	if out.Outcome != JoinUnknown {
		t.Fatalf("first call: %+v", out)
	}
	js, err := joinstate.Read(j.dir)
	if err != nil || js == nil || len(js.Guests) != 2 || js.Guests[0] != wuGuest1 {
		t.Fatalf("join.json: %+v %v", js, err)
	}
	opID := js.OpID
	// The service repeats the call with the recorded operation ID and
	// list; here it reaches the cluster's members (the admission they
	// gave, written back in place of the silent one).
	js.Admission = adm
	if err := joinstate.Write(j.dir, *js); err != nil {
		t.Fatal(err)
	}
	h.start(j)
	h.joined(j, 60*time.Second)
	if i, ok := l.svc.machine.OpIndex(joinOpID(j.id, opID)); !ok {
		t.Fatal("the member.join of the recorded operation ID is not applied")
	} else if mem, _ := l.svc.machine.Member(j.id); mem.JoinedIndex != i {
		t.Fatalf("joined at %d, the operation at %d", mem.JoinedIndex, i)
	}
	if got := ownedBy(l, j.id); len(got) != 2 {
		t.Fatalf("wu's guests: %v", got)
	}
	// A repeat with the same key now is the existing shortcut: accepted,
	// nothing written.
	before := l.svc.machine.View().AppliedIndex
	if _, err := l.svc.handleJoin(raftnet.Peer{NodeID: j.id, Pin: j.key.Pin()}, JoinRequest{OpID: opID, Guests: []clusterstate.JoinGuest{wuGuest1, wuGuest2}}); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if after := l.svc.machine.View().AppliedIndex; after != before {
		t.Fatalf("a repeat wrote %d entries", after-before)
	}
}

// F6, D4: the largest list D4 allows fits one Skep RPC (envelope
// included) as the join call's body, and its member.join entry fits 192
// KiB.
func TestJoinListLimitsFitOneEntryAndOneRPC(t *testing.T) {
	// 1000 guests whose names fill the byte limit, mostly with escaped
	// bytes ("<" is 6 bytes as JSON).
	list := make([]clusterstate.JoinGuest, clusterstate.MaxJoinGuests)
	for i := range list {
		list[i] = clusterstate.JoinGuest{ID: fmt.Sprintf("d0000000-0000-4000-8000-%012d", i), Kind: clusterstate.KindContainer, Name: fmt.Sprintf("%04d", i)}
	}
	base, _ := json.Marshal(list)
	extra := (clusterstate.MaxJoinGuestsBytes - len(base)) / len(list)
	for i := range list {
		list[i].Name += strings.Repeat("<", extra/6) + strings.Repeat("n", extra%6)
	}
	if err := clusterstate.CheckJoinGuestLimits(list); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(list)
	if len(gb) < clusterstate.MaxJoinGuestsBytes-1000 {
		t.Fatalf("the list is %d bytes, not at the limit", len(gb))
	}
	id := "d0000000-0000-4000-8000-ffffffffffff"
	body, _ := json.Marshal(JoinRequest{OpID: id, Guests: list})
	frame, _ := json.Marshal(struct {
		Op     string          `json:"op"`
		NodeID string          `json:"node_id"`
		Body   json.RawMessage `json:"body"`
	}{raftnet.OpJoin, id, body})
	if len(frame) > 256<<10 {
		t.Fatalf("the join call is %d bytes, over one Skep RPC", len(frame))
	}
	e, err := clusterstate.NewEntryVersion(clusterstate.TypeMemberJoin, 2, joinOpID(id, id), "node:"+id, id,
		clusterstate.MemberJoinV2{NodeID: id, Pin: "sha256:" + strings.Repeat("f", 64), At: 1 << 40, Guests: list})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := e.Encode()
	if len(raw) > clusterstate.MaxImportBytes {
		t.Fatalf("the entry is %d bytes, over 192 KiB", len(raw))
	}
	t.Logf("guests %d bytes, join call %d bytes, entry %d bytes", len(gb), len(frame), len(raw))
}

// ownSome makes x own one guest and one name reservation, through x's
// socket.
func ownSome(t *testing.T, x *tnode) {
	t.Helper()
	ctx := context.Background()
	if _, err := x.svc.GuestsWrite(ctx, guestReq(clusterstate.TypeGuestReserve, "o-r-"+x.name, `{"kind":"vm","name":"res-`+x.name+`","key":"k-`+x.name+`"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := x.svc.GuestsWrite(ctx, guestReq(clusterstate.TypeGuestAdd, "o-a-"+x.name, `{"id":"`+guestID1+`","kind":"container","name":"g-`+x.name+`","task_id":""}`)); err != nil {
		t.Fatal(err)
	}
}

// D22, D27: the removal of a member that owns guests: without the flag,
// with wrong counts and with missing counts it is refused before the
// status round (nothing written, same leader and term); with matching
// counts, through a follower, it is removed and its entries forgotten on
// every node.
func TestRemoveForgetGuests(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	ctx := context.Background()
	l := h.leader()
	x := follower(h)
	other := follower(h, x)
	ownSome(t, x)
	h.eventually("x's entries on the leader", 10*time.Second, func() bool { g, r := l.svc.machine.GuestsOwnedBy(x.id); return g == 1 && r == 1 })
	term, applied := l.st.Stats().Term, l.svc.machine.View().AppliedIndex
	for _, c := range []struct {
		name string
		req  localapi.RemoveRequest
		code string
		text string
	}{
		{"no flag", localapi.RemoveRequest{Name: x.name}, "refused", "owns 1 guests and 1 name reservations in the inventory; it cannot be removed while it owns any. To remove it and make the cluster forget them, run \"skep-cluster remove --forget-guests " + x.name + "\" as root"},
		{"wrong counts", localapi.RemoveRequest{Name: x.name, ForgetGuests: true, Guests: 1}, "refused", "was confirmed for 1 guests and 0 name reservations, and it owns 1 guests and 1 name reservations now; nothing was written; run the command again"},
		{"missing counts", localapi.RemoveRequest{Name: x.name, ForgetGuests: true}, "refused", "confirmed for 0 guests and 0 name reservations"},
		{"counts without the flag", localapi.RemoveRequest{Name: x.name, Guests: 1, Reservations: 1}, "invalid_request", "go with forget_guests"},
		{"a count below 0", localapi.RemoveRequest{Name: x.name, ForgetGuests: true, Guests: -1}, "invalid_request", "never below 0"},
	} {
		_, err := other.svc.Remove(ctx, c.req, "root")
		if opCode(err) != c.code || !strings.Contains(err.Error(), c.text) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if l.st.Stats().Term != term || l.svc.machine.View().AppliedIndex != applied || h.leader() != l {
		t.Fatal("a refused removal changed something")
	}
	res, err := other.svc.Remove(ctx, localapi.RemoveRequest{Name: x.name, ForgetGuests: true, Guests: 1, Reservations: 1}, "root")
	if err != nil || res.NodeID != x.id {
		t.Fatalf("the removal: %+v %v", res, err)
	}
	h.gone(x, 60*time.Second)
	for _, n := range h.running() {
		if n == x {
			continue
		}
		n := n
		h.eventually("x's entries forgotten on "+n.name, 10*time.Second, func() bool {
			g, r := n.svc.machine.GuestsOwnedBy(x.id)
			return g == 0 && r == 0
		})
	}
}

// D22, V7: the leader that owns guests is removed with matching counts:
// it hands the leadership over first and the new leader removes it. The
// new leader compares the counts itself: a passed removal with counts that
// are not the member's is refused there.
func TestRemoveTheLeaderThatOwnsGuests(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	ctx := context.Background()
	l := h.leader()
	ownSome(t, l)
	cid := l.svc.machine.View().ClusterID
	// What the old leader passes on after a hand-over, with wrong counts,
	// given to a member that is not the leader and to the leader.
	other := follower(h)
	wrong := forwardRemove{Name: l.name, Actor: "root", OpID: newID(t), ClusterID: cid, ForgetGuests: true, Guests: 1, Reservations: 0}
	body, _ := json.Marshal(wrong)
	if _, err := other.svc.handleRemove(ctx, raftnet.Peer{NodeID: l.id}, body); err == nil {
		t.Fatal("a member that is not the leader took the removal")
	}
	if _, err := l.svc.handleRemove(ctx, raftnet.Peer{NodeID: other.id}, body); err == nil || !strings.Contains(err.Error(), "was confirmed for 1 guests and 0 name reservations") {
		t.Fatalf("the leader with wrong counts: %v", err)
	}
	res, err := l.svc.Remove(ctx, localapi.RemoveRequest{Name: l.name, ForgetGuests: true, Guests: 1, Reservations: 1}, "root")
	if err != nil || !res.HandedOver {
		t.Fatalf("the removal of the leader: %+v %v", res, err)
	}
	h.gone(l, 60*time.Second)
	nl := h.leader()
	if g, r := nl.svc.machine.GuestsOwnedBy(l.id); g != 0 || r != 0 {
		t.Fatalf("the old leader's entries: %d %d", g, r)
	}
	// The new leader checked the counts against its own state machine: a
	// removal it is handed with other counts is refused (V7).
	x := follower(h)
	ownSome(t, x)
	h.eventually("x's entries on the new leader", 10*time.Second, func() bool { g, r := nl.svc.machine.GuestsOwnedBy(x.id); return g == 1 && r == 1 })
	wrong = forwardRemove{Name: x.name, Actor: "root", OpID: newID(t), ClusterID: cid, ForgetGuests: true, Guests: 2, Reservations: 1}
	body, _ = json.Marshal(wrong)
	if _, err := nl.svc.handleRemove(ctx, raftnet.Peer{NodeID: x.id}, body); err == nil || !strings.Contains(err.Error(), "confirmed for 2 guests and 1 name reservations, and it owns 1 guests and 1") {
		t.Fatalf("the new leader with wrong counts: %v", err)
	}
}

// D18, D27: a host that joined with guests and is still joining (a voter
// is down, so it is not promoted) refuses guest writes; it is removed with
// --forget-guests, its guests are forgotten, and "join --abandon" puts its
// one-node cluster back.
func TestRemoveAJoiningMemberWithGuestsAndAbandon(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe", "star")
	down := follower(h)
	h.stop(down)
	j := h.node("wu")
	h.start(j)
	h.eventually("wu leads its one-node cluster", 20*time.Second, func() bool { return j.svc.machine.View().ClusterID != "" })
	ownCluster, ownPin := j.svc.machine.View().ClusterID, j.key.Pin()
	l := h.leader()
	adm := h.admit(l, j)
	if out := joinWithGuests(h, j, adm, wuGuest1, wuGuest2); out.Outcome != JoinAccepted {
		t.Fatalf("join: %+v", out)
	}
	h.eventually("wu's guests on the leader", 20*time.Second, func() bool { return len(ownedBy(h.leader(), j.id)) == 2 })
	if st := stateOf(h.leader(), j.id); st != clusterstate.StateJoining {
		t.Fatalf("wu is %s", st)
	}
	ctx := context.Background()
	if _, err := j.svc.GuestsWrite(ctx, guestReq(clusterstate.TypeGuestRemove, "x-1", `{"id":"`+wuGuest1.ID+`"}`)); err == nil || !strings.Contains(err.Error(), "this node is joining") {
		t.Fatalf("a guest write of the joining node: %v", err)
	}
	l = h.leader()
	if _, err := l.svc.Remove(ctx, localapi.RemoveRequest{Name: j.name}, "root"); opCode(err) != "refused" || !strings.Contains(err.Error(), "--forget-guests") {
		t.Fatalf("removal without the flag: %v", err)
	}
	if _, err := l.svc.Remove(ctx, localapi.RemoveRequest{Name: j.name, ForgetGuests: true, Guests: 2}, "root"); err != nil {
		t.Fatalf("removal: %v", err)
	}
	h.gone(j, 60*time.Second)
	h.eventually("wu's guests forgotten", 10*time.Second, func() bool { return len(ownedBy(h.leader(), j.id)) == 0 })
	h.eventually("wu knows it was removed", 30*time.Second, func() bool { return removedMode(j) })
	h.stop(j)
	if out := StageAbandon(j.dir, j.cfg, j.key, 3*time.Second); out.Outcome != "restored" {
		t.Fatalf("abandon: %+v", out)
	}
	h.start(j)
	h.eventually("wu's own one-node cluster is back", 20*time.Second, func() bool {
		return j.st != nil && j.st.IsLeader() && j.svc.machine.View().ClusterID == ownCluster
	})
	if j.key.Pin() != ownPin || len(j.svc.machine.View().Members) != 1 {
		t.Fatal("not the host's own cluster")
	}
	h.start(down)
}
