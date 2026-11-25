package apidb

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/userapi"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/migrations"
)

const (
	clusterOne = "11111111-1111-4111-8111-111111111111"
	clusterTwo = "22222222-2222-4222-8222-222222222222"
	uAndy      = "00000000-0000-4000-8000-0000000000a1"
	uVera      = "00000000-0000-4000-8000-0000000000a2"
	hashOne    = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$b25l"
	hashTwo    = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$dHdv"
)

func openCopy(t *testing.T) (*sql.DB, *UserStore) {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := database.Migrate(ctx, db, migrations.API()); err != nil {
		t.Fatal(err)
	}
	return db, NewUserStore(db)
}

func cuser(id, name, role, hash string, epoch uint64) clusterstate.User {
	return clusterstate.User{ID: id, Username: name, RoleID: role, PasswordHash: hash, CredentialEpoch: epoch, CreatedAt: 100, UpdatedAt: 100}
}

func answer(cluster string, index uint64, users ...clusterstate.User) userapi.UsersAnswer {
	return userapi.UsersAnswer{ClusterID: cluster, UsersIndex: index, Users: users, LeaderKnown: true}
}

func addSessionRow(t *testing.T, db *sql.DB, user string, epoch int) {
	t.Helper()
	h := strings.Repeat("0", 63) + string(rune('a'+countRows(t, db, `SELECT COUNT(*) FROM sessions`)))
	now := "2026-11-25T00:00:00.000Z"
	if _, err := db.Exec(`INSERT INTO sessions (token_hash, user_id, kind, created_at, last_seen_at, expires_at, credential_epoch)
		VALUES (?, ?, 'bearer', ?, ?, '2099-01-01T00:00:00.000Z', ?)`, h, user, now, now, epoch); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func apply(t *testing.T, s *UserStore, a userapi.UsersAnswer) Applied {
	t.Helper()
	res, err := s.ApplyAnswer(context.Background(), a, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// Case 1: a copy with no stored cluster ID and an import done or not
// needed adopts the answer's cluster; sessions end only by the epoch rule.
// Then, for the same cluster ID, it never goes back to a lower index (V5),
// and a user whose epoch changed or who is gone loses its sessions, each
// audited by "system" with the cause.
func TestCopyAdoptsThenNeverGoesBack(t *testing.T) {
	db, s := openCopy(t)
	if cs, _ := s.CopyState(context.Background()); cs.Import != ImportNotNeeded || cs.ClusterID != "" {
		t.Fatalf("fresh copy: %+v", cs)
	}
	res := apply(t, s, answer(clusterOne, 5, cuser(uAndy, "andy", "admin", hashOne, 1), cuser(uVera, "vera", "viewer", hashOne, 1)))
	if !res.Changed || res.Replaced || res.UsersIndex != 5 {
		t.Fatalf("adopt: %+v", res)
	}
	addSessionRow(t, db, uAndy, 1)
	addSessionRow(t, db, uVera, 1)
	// A lower index (a restarted follower behind the copy) changes nothing.
	if res := apply(t, s, answer(clusterOne, 4)); res.Changed || countRows(t, db, `SELECT COUNT(*) FROM users`) != 2 {
		t.Fatalf("a lower index was applied: %+v", res)
	}
	if res := apply(t, s, answer(clusterOne, 5)); res.Changed {
		t.Fatal("the same index was applied again")
	}
	// A rehash (no epoch change) keeps vera's session; a password change
	// of andy ends andy's; vera's display name changes.
	vera := cuser(uVera, "vera", "viewer", hashTwo, 1)
	vera.DisplayName = "Vera V."
	res = apply(t, s, answer(clusterOne, 9, cuser(uAndy, "andy", "admin", hashTwo, 2), vera))
	if res.SessionsEnded != 1 || countRows(t, db, `SELECT COUNT(*) FROM sessions WHERE user_id = ?`, uVera) != 1 {
		t.Fatalf("epoch rule: %+v", res)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM audit_log WHERE action = 'session.revoke' AND actor_name = 'system'
		AND resource_id = ? AND details LIKE '%password change%'`, uAndy); n != 1 {
		t.Fatalf("%d revocation entries", n)
	}
	// vera is gone: her session ends, cause "user deleted".
	apply(t, s, answer(clusterOne, 12, cuser(uAndy, "andy", "admin", hashTwo, 2)))
	if countRows(t, db, `SELECT COUNT(*) FROM sessions`) != 0 || countRows(t, db, `SELECT COUNT(*) FROM users`) != 1 ||
		countRows(t, db, `SELECT COUNT(*) FROM audit_log WHERE details LIKE '%user deleted%'`) != 1 {
		t.Fatal("a deleted user kept a session or its row")
	}
	u, err := s.UserByName(context.Background(), "andy")
	if err != nil || u.CredentialEpoch != 2 || u.PasswordHash != hashTwo {
		t.Fatalf("andy: %+v %v", u, err)
	}
	// last_login_at is local: the copy never writes it.
	if err := s.RecordLogin(context.Background(), uAndy, time.Now()); err != nil {
		t.Fatal(err)
	}
	apply(t, s, answer(clusterOne, 13, cuser(uAndy, "andy", "admin", hashTwo, 2)))
	if u, _ := s.UserByName(context.Background(), "andy"); u.LastLoginAt == nil {
		t.Fatal("the copy cleared last_login_at")
	}
	// No hash in the access audit.
	if countRows(t, db, `SELECT COUNT(*) FROM audit_log WHERE details LIKE '%argon2id%'`) != 0 {
		t.Fatal("the access audit carries a hash")
	}
}

// Case 2: another cluster ID (a join, a wipe) replaces the users and
// deletes every session.
func TestCopyOfAnotherClusterReplaces(t *testing.T) {
	db, s := openCopy(t)
	apply(t, s, answer(clusterOne, 50, cuser(uAndy, "andy", "admin", hashOne, 1)))
	addSessionRow(t, db, uAndy, 1)
	// A lower index of another cluster is not "going back": it is
	// another copy.
	res := apply(t, s, answer(clusterTwo, 3, cuser(uVera, "andy", "admin", hashTwo, 1)))
	if !res.Replaced || res.SessionsEnded != 1 || countRows(t, db, `SELECT COUNT(*) FROM sessions`) != 0 {
		t.Fatalf("replace: %+v", res)
	}
	if u, err := s.UserByName(context.Background(), "andy"); err != nil || u.ID != uVera {
		t.Fatalf("andy after the replace: %+v %v", u, err)
	}
	// A wiped node's new cluster has no users: the copy is empty.
	apply(t, s, answer(clusterOne, 1))
	if countRows(t, db, `SELECT COUNT(*) FROM users`) != 0 {
		t.Fatal("a cluster without users left users in the copy")
	}
}

// Cases 3 and 4: an import pending writes nothing; superseded with no
// stored cluster ID replaces and deletes every session.
func TestCopyImportPendingAndSuperseded(t *testing.T) {
	db, s := openCopy(t)
	ctx := context.Background()
	// A 0.1 host: its own users, then migration 0006 found them.
	if _, err := db.Exec(`INSERT INTO users (id, username, password_hash, role_id) VALUES (?, 'old', ?, 'admin')`, uAndy, hashOne); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE cluster_copy SET import_state = 'pending'`); err != nil {
		t.Fatal(err)
	}
	addSessionRow(t, db, uAndy, 1)
	if res := apply(t, s, answer(clusterOne, 7, cuser(uVera, "vera", "admin", hashTwo, 1))); res.Changed {
		t.Fatal("a pending import let the copy change")
	}
	op, err := s.RecordImportOp(ctx, "op-1")
	if err != nil || op != "op-1" {
		t.Fatalf("record: %q %v", op, err)
	}
	if op, _ := s.RecordImportOp(ctx, "op-2"); op != "op-1" {
		t.Fatalf("a second record replaced the first: %q", op)
	}
	if err := s.FinishImport(ctx, ImportSuperseded); err != nil {
		t.Fatal(err)
	}
	res := apply(t, s, answer(clusterOne, 7, cuser(uVera, "vera", "admin", hashTwo, 1)))
	if !res.Replaced || countRows(t, db, `SELECT COUNT(*) FROM sessions`) != 0 || countRows(t, db, `SELECT COUNT(*) FROM users WHERE username = 'old'`) != 0 {
		t.Fatalf("superseded: %+v", res)
	}
	if err := s.FinishImport(ctx, ImportDone); err != nil {
		t.Fatal(err)
	}
	if cs, _ := s.CopyState(ctx); cs.Import != ImportSuperseded {
		t.Fatalf("a finished import moved again: %+v", cs)
	}
}

// Case "done": after the import, the copy adopts the cluster's users (the
// same IDs and epochs) and keeps the sessions.
func TestCopyAfterImportKeepsSessions(t *testing.T) {
	db, s := openCopy(t)
	ctx := context.Background()
	if _, err := db.Exec(`INSERT INTO users (id, username, password_hash, role_id, password_changed_at) VALUES (?, 'andy', ?, 'admin', '2026-01-01T00:00:00.000Z')`, uAndy, hashOne); err != nil {
		t.Fatal(err)
	}
	db.Exec(`UPDATE cluster_copy SET import_state = 'pending'`)
	addSessionRow(t, db, uAndy, 1)
	local, err := s.LocalImportUsers(ctx)
	if err != nil || len(local) != 1 || local[0].PasswordHash != hashOne || local[0].PasswordChangedAt == 0 {
		t.Fatalf("import: %+v %v", local, err)
	}
	// The import entry is index 3: "done" is recorded with the cluster
	// ID and index 2 (F1 of the 8c-1 review).
	if err := s.FinishImportDone(ctx, clusterOne, 2); err != nil {
		t.Fatal(err)
	}
	// An answer read before the import (no users) and applied after
	// "done": the same-cluster rule drops it; nothing is deleted.
	if res := apply(t, s, answer(clusterOne, 1)); res.Changed || countRows(t, db, `SELECT COUNT(*) FROM users`) != 1 ||
		countRows(t, db, `SELECT COUNT(*) FROM sessions`) != 1 {
		t.Fatalf("an answer read before the import: %+v", res)
	}
	res := apply(t, s, answer(clusterOne, 3, cuser(uAndy, "andy", "admin", hashOne, 1)))
	if !res.Changed || res.Replaced || res.SessionsEnded != 0 || countRows(t, db, `SELECT COUNT(*) FROM sessions`) != 1 {
		t.Fatalf("after import: %+v", res)
	}
	if err := s.FinishImportDone(ctx, "", 2); err == nil {
		t.Fatal("done without a cluster ID")
	}
}
