package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/audit"
	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/peerapi"
)

func testAPIPin(c string) string { return "sha256:" + strings.Repeat(c, 64) }

func apiPinOf(n *tnode, id string) string {
	m, _ := n.svc.machine.Member(id)
	return m.APIPin
}

func opError(t *testing.T, err error, code string) *localapi.OpError {
	t.Helper()
	var oe *localapi.OpError
	if !errors.As(err, &oe) || oe.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
	return oe
}

// D8, D9, D11, D12 (step 8e-1): the API key through the socket on a member
// and on the leader; the guard with a member of an earlier build, a member
// that does not answer, and every member answering; peers-read and
// guest-owner.
func TestAPIKeyGuardAndPeers(t *testing.T) {
	h := newHarness(t)
	nodes := h.cluster("smith", "langrishe", "star")
	h.eventually("three voters", 60*time.Second, func() bool { return len(voters(nodes[0])) == 3 })
	leader := h.leader()
	var member, third *tnode
	for _, n := range nodes {
		switch {
		case n == leader:
		case member == nil:
			member = n
		default:
			third = n
		}
	}
	ctx := context.Background()
	op := func(i int) string { return "0000000" + string(rune('0'+i)) + "-0000-4000-8000-000000000000" }

	// A member of an earlier build: refused on the leader, nothing written.
	third.svc.mu.Lock()
	third.svc.noSupports = true
	third.svc.mu.Unlock()
	_, err := member.svc.APIKey(ctx, peerapi.APIKeyRequest{Pin: testAPIPin("1"), OpID: op(1)})
	oe := opError(t, err, "unavailable")
	if !strings.Contains(oe.Message, third.name) || !strings.Contains(oe.Message, "earlier build") || !strings.Contains(oe.Message, "nothing was written") {
		t.Fatalf("message %q", oe.Message)
	}
	// The leader's own registration meets the same guard.
	if _, err := leader.svc.APIKey(ctx, peerapi.APIKeyRequest{Pin: testAPIPin("1"), OpID: op(2)}); err == nil {
		t.Fatal("the leader registered with a member of an earlier build")
	}
	third.svc.mu.Lock()
	third.svc.noSupports = false
	third.svc.mu.Unlock()
	// A member that does not answer.
	h.stop(third)
	_, err = member.svc.APIKey(ctx, peerapi.APIKeyRequest{Pin: testAPIPin("1"), OpID: op(3)})
	if oe := opError(t, err, "unavailable"); !strings.Contains(oe.Message, "did not answer") {
		t.Fatalf("message %q", oe.Message)
	}
	if apiPinOf(member, member.id) != "" {
		t.Fatal("a refused registration was written")
	}
	h.start(third)
	h.eventually(third.name+" back", 60*time.Second, func() bool { return third.st.Stats().LeaderID != "" })

	// Every member answers: a member's registration is passed to the
	// leader, which sets the node from the calling key.
	var res peerapi.APIKeyResult
	h.eventually("the member's API key registered", 30*time.Second, func() bool {
		res, err = member.svc.APIKey(ctx, peerapi.APIKeyRequest{Pin: testAPIPin("1"), OpID: op(4)})
		return err == nil
	})
	if res.Outcome != peerapi.APIKeyApplied {
		t.Fatalf("%+v", res)
	}
	for _, n := range nodes {
		h.eventually(n.name+" applied the API pin", 20*time.Second, func() bool { return apiPinOf(n, member.id) == testAPIPin("1") })
		var found bool
		for _, r := range records(t, n) {
			if r.Kind == audit.KindEntry && r.EntryType == clusterstate.TypeMemberAPIKey && r.NodeID == member.id && r.APIPin == testAPIPin("1") &&
				r.Actor == "node:"+member.id && r.Origin == member.id {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: no member.api_key record", n.name)
		}
	}
	// The same pin again: unchanged. The leader's own: applied. Another
	// node's API pin: refused with the entry's code.
	if res, err := member.svc.APIKey(ctx, peerapi.APIKeyRequest{Pin: testAPIPin("1"), OpID: op(5)}); err != nil || res.Outcome != peerapi.APIKeyUnchanged {
		t.Fatalf("again: %+v %v", res, err)
	}
	if res, err := leader.svc.APIKey(ctx, peerapi.APIKeyRequest{Pin: testAPIPin("2"), OpID: op(6)}); err != nil || res.Outcome != peerapi.APIKeyApplied {
		t.Fatalf("the leader's: %+v %v", res, err)
	}
	if _, err := third.svc.APIKey(ctx, peerapi.APIKeyRequest{Pin: testAPIPin("1"), OpID: op(7)}); err == nil {
		t.Fatal("another member's API pin was accepted")
	} else if oe := opError(t, err, clusterstate.CodeInvalid); !strings.Contains(oe.Message, "belongs to member") {
		t.Fatalf("message %q", oe.Message)
	}
	// Refusals before anything is written: a pin or an operation ID that
	// is not valid.
	for _, bad := range []peerapi.APIKeyRequest{{Pin: "x", OpID: op(8)}, {Pin: testAPIPin("3"), OpID: "not-a-uuid"}} {
		if _, err := member.svc.APIKey(ctx, bad); err == nil {
			t.Fatalf("%+v accepted", bad)
		} else {
			opError(t, err, "invalid_request")
		}
	}

	// peers-read: every member entry with its API pin, as a server.
	a, err := third.svc.PeersRead(ctx, 0, 0)
	if err != nil || !a.Current || a.NodeID != third.id || a.ListenAddress != third.cfg.ListenAddress || len(a.Peers) != 3 {
		t.Fatalf("peers %+v %v", a, err)
	}
	p, ok := a.Find(member.id)
	if !ok || p.APIPin != testAPIPin("1") || !p.RaftServer || p.State != clusterstate.StateMember || !peerapi.Usable(p, third.id) {
		t.Fatalf("peer %+v", p)
	}
	if self, _ := a.Self(); peerapi.Usable(self, third.id) {
		t.Fatal("this node is usable to itself")
	}
	// The long poll answers at once on another index and waits otherwise;
	// a membership change wakes it.
	start := time.Now()
	if _, err := third.svc.PeersRead(ctx, a.MembershipIndex, time.Second); err != nil || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("did not wait: %v %s", err, time.Since(start))
	}
	woke := make(chan peerapi.PeersAnswer, 1)
	go func() {
		b, _ := third.svc.PeersRead(ctx, a.MembershipIndex, 5*time.Second)
		woke <- b
	}()
	time.Sleep(200 * time.Millisecond)
	if _, err := third.svc.APIKey(ctx, peerapi.APIKeyRequest{Pin: testAPIPin("3"), OpID: op(9)}); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-woke:
		if b.MembershipIndex == a.MembershipIndex {
			t.Fatal("woke without a change")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("the long poll was not woken by the membership change")
	}

	// guest-owner.
	g, err := member.svc.GuestOwner(ctx, "99999999-0000-4000-8000-000000000099")
	if err != nil || g.Found || g.ClusterID == "" || !g.LeaderKnown {
		t.Fatalf("unknown guest: %+v %v", g, err)
	}
}

// D9: a node without a listen address registers nothing.
func TestAPIKeyWithoutListenAddress(t *testing.T) {
	s := New(Options{Config: clusterconf.Config{NodeID: "a1a1a1a1-0000-4000-8000-00000000000a", NodeName: "solo"}})
	_, err := s.APIKey(context.Background(), peerapi.APIKeyRequest{Pin: testAPIPin("1"), OpID: "a1a1a1a1-0000-4000-8000-00000000000b"})
	opError(t, err, "no_listen_address")
}

// D7: supports.
func TestSupports(t *testing.T) {
	for _, c := range []struct {
		body string
		want bool
	}{
		{`{"type":"member.api_key","version":1}`, true},
		{`{"type":"member.api_key","version":2}`, false},
		{`{"type":"member.join","version":2}`, true},
		{`{"type":"nothing","version":1}`, false},
		{`{"type":"member.api_key","version":0}`, false},
	} {
		a, err := handleSupports([]byte(c.body))
		if err != nil || a.(SupportsAnswer).Supported != c.want {
			t.Errorf("%s: %+v %v", c.body, a, err)
		}
	}
	if _, err := handleSupports([]byte(`{"type":"x","version":1,"extra":1}`)); err == nil {
		t.Error("an unknown field was accepted")
	}
}
