package service

import (
	"context"
	"testing"

	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/memberapi"
	"github.com/greenbeanorg/skep/internal/ids"
)

// D22 of step 8c-2: the actor the socket accepts from skep-api for a
// member operation is the "user:" form of the users-write rule.
func TestMemberActorFollowsTheUsersWriteRule(t *testing.T) {
	for _, a := range []string{"user:andy", "user:a", "user:a.b-c_d", "user:abcdefghijklmnopqrstuvwxyz012345",
		"user:", "user:Andy", "user:1a", "local:0", "root", "user:andy\n", "user:abcdefghijklmnopqrstuvwxyz0123456", " user:andy"} {
		want := actorRE.MatchString(a) && len(a) > 5 && a[:5] == "user:"
		if got := memberapi.ValidUserActor(a); got != want {
			t.Errorf("%q: memberapi %v, users-write's user: form %v", a, got, want)
		}
	}
}

// D24 of step 8c-2: a cluster ID given with a removal is compared on the
// node that receives it and again on the leader; a difference is
// cluster_id_mismatch and nothing is written.
func TestRemoveClusterIDMismatch(t *testing.T) {
	h := newHarness(t)
	nodes := h.cluster("smith", "langrishe", "star")
	leader := h.leader()
	var follower *tnode
	for _, n := range nodes {
		if n != leader {
			follower = n
		}
	}
	before := leader.st.Stats().CommitIndex
	wrong := "00000000-0000-4000-8000-000000000001"
	for _, on := range []*tnode{follower, leader} {
		_, err := on.svc.Remove(context.Background(), localapi.RemoveRequest{Name: follower.name, ClusterID: wrong}, "root")
		if opCode(err) != CodeClusterIDMismatch {
			t.Fatalf("on %s: %v", on.name, err)
		}
	}
	// The leader's own comparison, for a request a member passed on.
	opID, _ := ids.NewUUID()
	_, err := leader.svc.removeAsLeader(context.Background(), forwardRemove{Name: follower.name, Actor: "user:andy", OpID: opID, ClusterID: wrong}, follower.id)
	if opCode(err) != CodeClusterIDMismatch {
		t.Fatalf("on the leader: %v", err)
	}
	if after := leader.st.Stats().CommitIndex; after != before {
		t.Fatalf("written: commit %d -> %d", before, after)
	}
	// The right one goes ahead, with the user as the actor.
	res, err := follower.svc.Remove(context.Background(), localapi.RemoveRequest{Name: follower.name, ClusterID: leader.svc.machine.View().ClusterID}, "user:andy")
	if err != nil || res.Name != follower.name {
		t.Fatalf("%+v %v", res, err)
	}
}

// Step 8c-2 lab: after a hand-over the removal is passed on again while
// the new leader says nothing was written.
func TestHandoverRetryable(t *testing.T) {
	for code, want := range map[string]bool{"unavailable": true, "not_leader": true, "no_leader": true,
		"outcome_unknown": false, "not_committed": false, "refused": false, "cluster_id_mismatch": false} {
		if got := handoverRetryable(&localapi.OpError{Code: code}); got != want {
			t.Errorf("%s: %v", code, got)
		}
	}
	if handoverRetryable(context.Canceled) {
		t.Error("a plain error is not retried")
	}
}
