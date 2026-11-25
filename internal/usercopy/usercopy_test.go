package usercopy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/apidb"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/userapi"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/migrations"
	"github.com/greenbeanorg/skep/pkg/models"
)

// answerSocket serves users-read with a fixed answer on a Unix socket: a
// stand-in for a node in a given state, for this unit test only.
func answerSocket(t *testing.T, a userapi.UsersAnswer) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "uc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(a)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return path
}

// D11, D17: an answer from a joining or removed node, a stalled machine
// or a node without a leader is no quorum. The copy applies nothing of a
// joining or removed node's answer, users and hashes included; a stalled
// or leaderless node's users are a committed prefix of the log, and are
// applied (F2 of the 8c-1 review).
func TestNoQuorumAnswersAreNotApplied(t *testing.T) {
	ctx := context.Background()
	u := clusterstate.User{ID: "00000000-0000-4000-8000-000000000001", Username: "ghost", RoleID: "admin",
		PasswordHash: "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$Z2hvc3Q", CredentialEpoch: 1, CreatedAt: 1, UpdatedAt: 1}
	for name, a := range map[string]userapi.UsersAnswer{
		"joining":   {ClusterID: "c", UsersIndex: 9, Users: []clusterstate.User{u}, LeaderKnown: true, Joining: true},
		"removed":   {ClusterID: "c", UsersIndex: 9, Users: []clusterstate.User{u}, LeaderKnown: true, Removed: true},
		"stalled":   {ClusterID: "c", UsersIndex: 9, Users: []clusterstate.User{u}, LeaderKnown: true, Stalled: true},
		"no leader": {ClusterID: "c", UsersIndex: 9, Users: []clusterstate.User{u}},
	} {
		db, err := database.Open(ctx, filepath.Join(t.TempDir(), "api.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.Migrate(ctx, db, migrations.API()); err != nil {
			t.Fatal(err)
		}
		store := apidb.NewUserStore(db)
		c := New(store, userapi.NewClient(answerSocket(t, a)), slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err := c.SyncOnce(ctx, 0); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		users, _ := store.ListUsers(ctx)
		want := 0
		if !a.Joining && !a.Removed {
			want = 1
		}
		if c.Quorum() || len(users) != want || c.Health(ctx) == "" {
			t.Fatalf("%s: quorum %v, %d users in the copy", name, c.Quorum(), len(users))
		}
		db.Close()
	}
	// Unreachable: no quorum.
	db, _ := database.Open(ctx, filepath.Join(t.TempDir(), "api.db"))
	defer db.Close()
	database.Migrate(ctx, db, migrations.API())
	c := New(apidb.NewUserStore(db), userapi.NewClient(filepath.Join(t.TempDir(), "absent")), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.SyncOnce(ctx, 0); err == nil || c.Quorum() || c.Problem() != "skep-cluster is not reachable on its socket" {
		t.Fatalf("unreachable: %v %q", err, c.Problem())
	}
}

// fakeNode serves users-read with a switchable answer, and answers every
// users-write 503 no_leader: a node whose leader is gone.
type fakeNode struct {
	mu sync.Mutex
	a  userapi.UsersAnswer
}

func (f *fakeNode) set(a userapi.UsersAnswer) { f.mu.Lock(); f.a = a; f.mu.Unlock() }

func (f *fakeNode) serve(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "uc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == userapi.PathUsersWrite {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(models.ErrorResponse{Error: models.ErrorBody{Code: "no_leader", Message: "the cluster has no leader now"}})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(f.a)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return path
}

// F2 of the 8c-1 review: a node whose state machine holds a disable and a
// password change, and then has no leader. Its answer is a committed prefix
// of the log: the copy takes both, the users' sessions end, the disabled
// administrator gets no degraded login, and writes still answer no_quorum.
func TestNoLeaderAnswerIsApplied(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := database.Migrate(ctx, db, migrations.API()); err != nil {
		t.Fatal(err)
	}
	hasher, err := auth.NewHasher(auth.MinParams, 2, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	const pw, pw2 = "correct horse battery staple", "another passphrase entirely"
	h1, _ := hasher.Hash(ctx, pw)
	h2, _ := hasher.Hash(ctx, pw2)
	const cid = "11111111-1111-4111-8111-111111111111"
	andy := clusterstate.User{ID: "00000000-0000-4000-8000-0000000000a1", Username: "andy", RoleID: "admin", PasswordHash: h1, CredentialEpoch: 1, CreatedAt: 1, UpdatedAt: 1}
	bob := clusterstate.User{ID: "00000000-0000-4000-8000-0000000000a2", Username: "bob", RoleID: "admin", PasswordHash: h1, CredentialEpoch: 1, CreatedAt: 1, UpdatedAt: 1}
	olga := clusterstate.User{ID: "00000000-0000-4000-8000-0000000000a3", Username: "olga", RoleID: "operator", PasswordHash: h1, CredentialEpoch: 1, CreatedAt: 1, UpdatedAt: 1}
	node := &fakeNode{}
	node.set(userapi.UsersAnswer{ClusterID: cid, UsersIndex: 5, Users: []clusterstate.User{andy, bob, olga}, LeaderKnown: true})
	store := apidb.NewUserStore(db)
	c := New(store, userapi.NewClient(node.serve(t)), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.SyncOnce(ctx, 0); err != nil || !c.Quorum() {
		t.Fatalf("with a leader: %v %q", err, c.Problem())
	}
	sessions := apidb.NewSessionStore(db)
	authn, err := auth.NewAuthenticator(auth.AuthenticatorOptions{Users: store, Writer: NewWriter(c), Sessions: sessions, Hasher: hasher,
		Audit: apidb.NewAccessLog(db), Throttle: auth.DefaultThrottleConfig(),
		Session: auth.SessionConfig{IdleTimeout: auth.DefaultIdleTimeout, AbsoluteTimeout: auth.DefaultAbsoluteTimeout},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	login := func(name, pass string) (auth.LoginResult, error) {
		return authn.Login(ctx, auth.LoginRequest{Username: name, Password: pass, Kind: auth.BearerSession, SourceIP: "192.0.2.50"})
	}
	var tokens []string
	for _, n := range []string{"andy", "olga", "bob"} {
		res, err := login(n, pw)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		tokens = append(tokens, res.Token)
	}

	// The state machine applied the disable and the password change; then
	// the leader went away.
	andy.Disabled, andy.CredentialEpoch = true, 2
	olga.PasswordHash, olga.CredentialEpoch = h2, 2
	node.set(userapi.UsersAnswer{ClusterID: cid, UsersIndex: 7, Users: []clusterstate.User{andy, bob, olga}})
	if err := c.SyncOnce(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if c.Quorum() || c.Problem() != "the cluster has no leader" {
		t.Fatalf("quorum after the leader went: %v %q", c.Quorum(), c.Problem())
	}
	if cs, _ := store.CopyState(ctx); cs.UsersIndex != 7 {
		t.Fatalf("the copy did not take the answer: %+v", cs)
	}
	for i, n := range []string{"andy", "olga"} {
		if _, err := authn.Authenticate(ctx, tokens[i], auth.BearerSession); err == nil {
			t.Errorf("%s's session survived", n)
		}
	}
	if _, err := authn.Authenticate(ctx, tokens[2], auth.BearerSession); err != nil {
		t.Errorf("bob's session ended: %v", err)
	}
	if res, err := login("andy", pw); err == nil {
		t.Fatalf("the disabled administrator logged in: %+v", res.Session)
	}
	if res, err := login("bob", pw); err != nil || !res.Session.Degraded {
		t.Fatalf("an enabled administrator without quorum: %+v %v", res.Session, err)
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err = NewWriter(c).Write(wctx, auth.UserWrite{Type: auth.WriteDelete, Actor: auth.Actor{Name: "local:0"}, Data: map[string]any{"id": olga.ID}})
	var me *models.Error
	if !errors.As(err, &me) || me.Code != auth.CodeNoQuorum {
		t.Fatalf("a write without a leader: %v", err)
	}
}

// F6a of the 8c-1 review: a failed apply is logged once per change of
// error, shown by the health check, and counts as no quorum while it
// lasts. F6g: Problem names a failed dial only for a failed dial.
func TestApplyFailureIsNoQuorum(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := database.Migrate(ctx, db, migrations.API()); err != nil {
		t.Fatal(err)
	}
	// A role the copy's schema does not have: the insert fails.
	bad := clusterstate.User{ID: "00000000-0000-4000-8000-0000000000b1", Username: "bad", RoleID: "writer",
		PasswordHash: "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$YmFk", CredentialEpoch: 1, CreatedAt: 1, UpdatedAt: 1}
	good := bad
	good.RoleID = "admin"
	node := &fakeNode{}
	node.set(userapi.UsersAnswer{ClusterID: "c", UsersIndex: 3, Users: []clusterstate.User{bad}, LeaderKnown: true})
	var logs bytes.Buffer
	c := New(apidb.NewUserStore(db), userapi.NewClient(node.serve(t)), slog.New(slog.NewTextHandler(&logs, nil)))
	for range 3 {
		if err := c.SyncOnce(ctx, 0); err == nil {
			t.Fatal("the bad answer applied")
		}
	}
	if n := strings.Count(logs.String(), "cannot apply the cluster's users"); n != 1 {
		t.Fatalf("the failed apply was logged %d times", n)
	}
	if c.Quorum() || !strings.Contains(c.Health(ctx), "could not be applied") || strings.Contains(logs.String(), "$argon2id$") {
		t.Fatalf("quorum %v, health %q", c.Quorum(), c.Health(ctx))
	}
	node.set(userapi.UsersAnswer{ClusterID: "c", UsersIndex: 4, Users: []clusterstate.User{good}, LeaderKnown: true})
	if err := c.SyncOnce(ctx, 0); err != nil || !c.Quorum() || c.Health(ctx) != "" {
		t.Fatalf("after a good answer: %v %q", err, c.Problem())
	}

	// F6g: an error answer to users-read gives its code.
	c = New(apidb.NewUserStore(db), userapi.NewClient(answerStatusSocket(t)), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.SyncOnce(ctx, 0); err == nil || c.Problem() != "skep-cluster answered forbidden" {
		t.Fatalf("an error answer: %v %q", err, c.Problem())
	}
}

// answerStatusSocket answers users-read 403 forbidden.
func answerStatusSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "uc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(models.ErrorResponse{Error: models.ErrorBody{Code: "forbidden", Message: "not this account"}})
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return path
}

// F6b and V3 of the 8c-1 review: the leader's "forbidden" (a member being
// removed) and a removed node's "removed" are transient for the writer:
// after the repeats, 503 no_quorum, never 403 or 409.
func TestForbiddenAndRemovedAreTransient(t *testing.T) {
	for _, code := range []string{"forbidden", "removed"} {
		transient, unknown := isTransient(&userapi.Error{HTTPStatus: 409, Code: code})
		if !transient || unknown {
			t.Errorf("%s: transient %v, unknown %v", code, transient, unknown)
		}
	}
	if transient, _ := isTransient(&userapi.Error{HTTPStatus: 409, Code: "username_taken"}); transient {
		t.Error("a refusal is transient")
	}
}
