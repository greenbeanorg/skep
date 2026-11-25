package usercopy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/greenbeanorg/skep/internal/apidb"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/userapi"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/migrations"
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
// or a node without a leader is no quorum, and the copy applies nothing of
// it, users and hashes included.
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
		if c.Quorum() || len(users) != 0 || c.Health(ctx) == "" {
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
