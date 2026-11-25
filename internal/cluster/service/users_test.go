package service

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/audit"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/userapi"
)

const (
	testHash  = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA"
	testHash2 = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHQ$b3RoZXJvdGhlcm90aGVyb3Ro"
	userID1   = "00000000-0000-4000-8000-0000000000c1"
	userID2   = "00000000-0000-4000-8000-0000000000c2"
	userID3   = "00000000-0000-4000-8000-0000000000c3"
)

func createReq(op, id, name, role string) localapi.UsersWriteRequest {
	return localapi.UsersWriteRequest{Type: "user.create", OpID: op, Actor: "user:andy",
		Data: []byte(`{"id":"` + id + `","username":"` + name + `","display_name":"","password_hash":"` + testHash + `","role_id":"` + role + `"}`)}
}

// noHashIn fails the test if the node's log lines or cluster audit carry a
// password hash (D10; V3).
func noHashIn(t *testing.T, n *tnode) {
	t.Helper()
	if strings.Contains(n.logs.String(), "$argon2id$") {
		t.Errorf("%s: a log line carries a password hash", n.name)
	}
	files, _ := filepath.Glob(filepath.Join(audit.Dir(n.dir), "audit-*.jsonl"))
	if len(files) == 0 {
		t.Errorf("%s: no audit segment", n.name)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("$argon2id$")) {
			t.Errorf("%s: %s carries a password hash", n.name, f)
		}
	}
}

// A user write through a follower reaches the leader as users-write and is
// applied on every node; a repeat with the same operation ID is a
// duplicate with the first index; refusals carry their codes; the last
// administrator is kept across two nodes; nothing logs or audits a hash.
func TestUsersThroughAFollowerAndTheLastAdministrator(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe")
	ctx := context.Background()
	l, x := h.leader(), follower(h)

	res, err := x.svc.UsersWrite(ctx, createReq("op-c1", userID1, "andy", "admin"))
	if err != nil || res.Outcome != userapi.WriteApplied || res.Index == 0 || res.User == nil || res.User.Username != "andy" || res.User.CredentialEpoch != 1 {
		t.Fatalf("create through the follower: %+v %v", res, err)
	}
	again, err := x.svc.UsersWrite(ctx, createReq("op-c1", userID1, "andy", "admin"))
	if err != nil || again.Outcome != userapi.WriteDuplicate || again.Index != res.Index {
		t.Fatalf("repeat: %+v %v", again, err)
	}
	if _, err := l.svc.UsersWrite(ctx, createReq("op-c2", userID2, "bob", "admin")); err != nil {
		t.Fatal(err)
	}
	if _, err := x.svc.UsersWrite(ctx, createReq("op-c3", userID3, "carl", "writer")); opCode(err) != "unknown_role" {
		t.Fatalf("unknown role through the follower: %v", err)
	}
	if _, err := x.svc.UsersWrite(ctx, createReq("op-c4", userID3, "bob", "viewer")); opCode(err) != "username_taken" {
		t.Fatalf("taken name: %v", err)
	}
	h.eventually("both nodes have two users", 10*time.Second, func() bool {
		return len(l.svc.machine.Users().Users) == 2 && l.svc.machine.UsersIndex() == x.svc.machine.UsersIndex()
	})

	// Two nodes delete one of the two administrators each, at once: the
	// state machine applies one and refuses the other.
	del := func(n *tnode, op, id string) error {
		_, err := n.svc.UsersWrite(ctx, localapi.UsersWriteRequest{Type: "user.delete", OpID: op, Actor: "local:0", Data: []byte(`{"id":"` + id + `"}`)})
		return err
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = del(l, "op-d1", userID1) }()
	go func() { defer wg.Done(); errs[1] = del(x, "op-d2", userID2) }()
	wg.Wait()
	ok, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case opCode(err) == "last_admin":
			refused++
		default:
			t.Errorf("delete: %v", err)
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("deletes: %d applied, %d refused (%v)", ok, refused, errs)
	}
	if v := l.svc.machine.Users(); len(v.Users) != 1 || v.Users[0].RoleID != "admin" {
		t.Fatalf("users after: %+v", v)
	}

	// users-read waits for a change, and a write ends the wait.
	idx := x.svc.machine.UsersIndex()
	start := time.Now()
	if a, _ := x.svc.UsersRead(ctx, idx, 300*time.Millisecond); a.UsersIndex != idx || time.Since(start) < 250*time.Millisecond || !a.LeaderKnown {
		t.Fatalf("an unchanged read did not wait: %+v", a)
	}
	done := make(chan localapi.UsersAnswer, 1)
	go func() { a, _ := x.svc.UsersRead(ctx, idx, 5*time.Second); done <- a }()
	time.Sleep(100 * time.Millisecond)
	if _, err := l.svc.UsersWrite(ctx, localapi.UsersWriteRequest{Type: "user.password", OpID: "op-p", Actor: "local:0",
		Data: []byte(`{"id":"` + l.svc.machine.Users().Users[0].ID + `","password_hash":"` + testHash2 + `"}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-done:
		if a.UsersIndex <= idx || len(a.Users) != 1 || a.Users[0].CredentialEpoch != 2 {
			t.Fatalf("woken read: %+v", a)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("a write did not end the wait")
	}
	for _, n := range []*tnode{l, x} {
		noHashIn(t, n)
		if rep, err := audit.Verify(audit.Dir(n.dir)); err != nil || rep.Break != nil {
			t.Fatalf("%s: audit verify: %+v %v", n.name, rep.Break, err)
		}
	}
}

// A joining node replays the log's history, users included: its users-read
// answers no users (D11), so skep-api's copy never shows a user or a hash
// from the history.
func TestJoiningNodeAnswersNoUsers(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.cluster("smith", "langrishe")
	ctx := context.Background()
	l := h.leader()
	if _, err := l.svc.UsersWrite(ctx, createReq("op-c1", userID1, "andy", "admin")); err != nil {
		t.Fatal(err)
	}
	j := h.node("star")
	h.start(j)
	// The leader stops after join step 2: star stays joining while it
	// receives the log.
	l.svc.mu.Lock()
	l.svc.afterJoinStep = func(step int, _ string) bool { return step == 2 }
	l.svc.mu.Unlock()
	if o := h.join(j, h.admit(l, j)); o.Outcome != JoinAccepted {
		t.Fatalf("join: %+v", o)
	}
	h.eventually("star has replayed the user", 20*time.Second, func() bool { return j.svc.machine.UsersIndex() > 0 })
	a, err := j.svc.UsersRead(ctx, 0, 0)
	if err != nil || !a.Joining || len(a.Users) != 0 || a.UsersIndex != 0 {
		t.Fatalf("joining node's users-read: %+v %v", a, err)
	}
	if _, err := j.svc.UsersWrite(ctx, createReq("op-x", userID2, "bob", "viewer")); opCode(err) != "no_leader" {
		t.Fatalf("a write on a joining node: %v", err)
	}
}
