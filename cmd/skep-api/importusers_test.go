package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/greenbeanorg/skep/internal/apidb"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/clustertest"
	"github.com/greenbeanorg/skep/internal/cluster/userapi"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/usercopy"
	"github.com/greenbeanorg/skep/migrations"
	"github.com/greenbeanorg/skep/pkg/models"
)

// configFor writes a configuration naming a given cluster socket.
func configFor(t *testing.T, socket string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.json")
	if err := os.WriteFile(path, []byte(`{"auth": {"argon2": {"memory_kib": 19456, "passes": 2}}, "cluster_socket": "`+socket+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// oldHost makes dataDir an upgraded 0.1 host: its own users in api.db,
// which migration api/0006 marks pending (the users are inserted before it
// runs, as 0.1 wrote them).
func oldHost(t *testing.T, dataDir string, names ...string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(dataDir, "api", "api.db")
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := database.Migrate(ctx, db, migrationsBefore(t, 6)); err != nil {
		t.Fatal(err)
	}
	for i, n := range names {
		role := "viewer"
		if i == 0 {
			role = "admin"
		}
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		if _, err := db.Exec(`INSERT INTO users (id, username, display_name, password_hash, role_id, password_changed_at) VALUES (?, ?, ?, ?, ?, '2026-01-02T03:04:05.000Z')`,
			id, n, strings.Repeat("d", 128), "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$"+n, role); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.Migrate(ctx, db, migrations.API()); err != nil {
		t.Fatal(err)
	}
}

// migrationsBefore is the API migrations below number n: the schema a 0.1
// host had.
func migrationsBefore(t *testing.T, n int) fstest.MapFS {
	t.Helper()
	out := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.API(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		var num int
		fmt.Sscanf(e.Name(), "%04d", &num)
		if num < n {
			b, _ := fs.ReadFile(migrations.API(), e.Name())
			out[e.Name()] = &fstest.MapFile{Data: b}
		}
	}
	return out
}

func copyState(t *testing.T, dataDir string) apidb.CopyState {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "api", "api.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cs, err := apidb.NewUserStore(db).CopyState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func runCmd(t *testing.T, args []string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	e := testEnv(&out, io.Discard)
	e.uid = 0
	err := run(context.Background(), args, e)
	return out.String(), err
}

// D14: before the import a user write is refused with
// users_import_pending; the import makes the host's users the cluster's,
// and the copy adopts them; a rerun says it is done.
func TestImportUsers(t *testing.T) {
	node := clustertest.Start(t)
	dataDir := t.TempDir()
	oldHost(t, dataDir, "andy", "vera")
	base := []string{"-dev", "-config", configFor(t, node.Socket), "-data-dir", dataDir}
	if cs := copyState(t, dataDir); cs.Import != apidb.ImportPending {
		t.Fatalf("after migration 0006: %+v", cs)
	}
	e, _, _ := adminEnv(testPassword + "\n")
	if err := run(context.Background(), append([]string{"admin", "create", "--username", "nora", "--password-stdin"}, base...), e); err == nil ||
		!strings.Contains(err.Error(), "users_import_pending") || !strings.Contains(err.Error(), "import-users") {
		t.Fatalf("a create before the import: %v", err)
	}
	out, err := runCmd(t, append([]string{"admin", "import-users"}, base...))
	if err != nil || !strings.Contains(out, "2 users are now the cluster's") {
		t.Fatalf("import: %q %v", out, err)
	}
	cs := copyState(t, dataDir)
	if cs.Import != apidb.ImportDone || cs.ClusterID != node.ClusterID || cs.UsersIndex == 0 || cs.ImportOpID == "" {
		t.Fatalf("after the import: %+v", cs)
	}
	if out, err := runCmd(t, append([]string{"admin", "import-users"}, base...)); err != nil || !strings.Contains(out, "imported already") {
		t.Fatalf("rerun: %q %v", out, err)
	}
	// The cluster's audit names the users, never a hash.
	files, _ := filepath.Glob(filepath.Join(node.Dir, "audit", "audit-*.jsonl"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if bytes.Contains(b, []byte("$argon2id$")) || (!bytes.Contains(b, []byte(`"users.import"`)) && f == files[len(files)-1]) {
			t.Fatalf("%s: a hash, or no import record", f)
		}
	}
}

// V4: a crash between recording the operation ID and sending it, and
// between the entry's apply and the state "done", is finished by a rerun:
// the same operation ID, applied once.
func TestImportUsersCrashPoints(t *testing.T) {
	node := clustertest.Start(t)
	ctx := context.Background()
	for _, point := range []string{"recorded", "applied"} {
		t.Run(point, func(t *testing.T) {
			dataDir := t.TempDir()
			oldHost(t, dataDir, "andy")
			base := []string{"-dev", "-config", configFor(t, node.Socket), "-data-dir", dataDir}
			db, err := database.Open(ctx, filepath.Join(dataDir, "api", "api.db"))
			if err != nil {
				t.Fatal(err)
			}
			store := apidb.NewUserStore(db)
			op, err := store.RecordImportOp(ctx, "import-"+point)
			if err != nil {
				t.Fatal(err)
			}
			if point == "applied" {
				// The command sent the entry and stopped before it
				// recorded "done".
				local, _ := store.LocalImportUsers(ctx)
				data, _ := json.Marshal(clusterstate.UsersImport{Users: local})
				w := usercopy.NewCommandWriter(usercopy.New(store, userapi.NewClient(node.Socket), slog.New(slog.NewTextHandler(io.Discard, nil))))
				if _, err := w.Send(ctx, userapi.UsersWriteRequest{Type: clusterstate.TypeUsersImport, OpID: op, Actor: "local:0", Data: data}, 10*time.Second); err != nil {
					t.Fatal(err)
				}
			}
			db.Close()
			out, err := runCmd(t, append([]string{"admin", "import-users"}, base...))
			if err != nil || !strings.Contains(out, "operation "+op) {
				t.Fatalf("rerun after %s: %q %v", point, out, err)
			}
			if cs := copyState(t, dataDir); cs.Import != apidb.ImportDone || cs.ClusterID != node.ClusterID {
				t.Fatalf("after %s: %+v", point, cs)
			}
		})
		// Only the first subtest imports: the cluster now has users, so
		// the second one runs against the same cluster only if it
		// replays its own operation.
		if point == "recorded" {
			node.Close()
			node = clustertest.Start(t)
		}
	}
}

// D14: on a second host the cluster has users already: without --yes
// nothing changes; with it, the cluster's users replace the host's and its
// sessions end.
func TestImportUsersSuperseded(t *testing.T) {
	node := clustertest.Start(t)
	first, second := t.TempDir(), t.TempDir()
	oldHost(t, first, "andy")
	oldHost(t, second, "bob", "vera")
	cfg := configFor(t, node.Socket)
	if _, err := runCmd(t, []string{"admin", "import-users", "-dev", "-config", cfg, "-data-dir", first}); err != nil {
		t.Fatal(err)
	}
	addSession(t, second, "00000000-0000-4000-8000-000000000001")
	base := []string{"-dev", "-config", cfg, "-data-dir", second}
	out, err := runCmd(t, append([]string{"admin", "import-users"}, base...))
	if err == nil || !strings.Contains(out, "replace this host's: bob, vera") || copyState(t, second).Import != apidb.ImportPending {
		t.Fatalf("without --yes: %q %v", out, err)
	}
	if out, err := runCmd(t, append([]string{"admin", "import-users", "--yes"}, base...)); err != nil || !strings.Contains(out, "uses the cluster's users") {
		t.Fatalf("with --yes: %q %v", out, err)
	}
	db := openAPIDB(t, second)
	if cs := copyState(t, second); cs.Import != apidb.ImportSuperseded || cs.ClusterID != node.ClusterID {
		t.Fatalf("superseded: %+v", cs)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM users WHERE username = 'andy'`); n != 1 || countRows(t, db, `SELECT COUNT(*) FROM users`) != 1 ||
		countRows(t, db, `SELECT COUNT(*) FROM sessions`) != 0 {
		t.Fatal("the cluster's users did not replace the host's")
	}
}

// D7: an import larger than one entry may be is refused before anything
// is recorded or sent.
func TestImportUsersTooLarge(t *testing.T) {
	node := clustertest.Start(t)
	dataDir := t.TempDir()
	names := make([]string, 700)
	for i := range names {
		names[i] = fmt.Sprintf("user%03d", i)
	}
	oldHost(t, dataDir, names...)
	_, err := runCmd(t, []string{"admin", "import-users", "-dev", "-config", configFor(t, node.Socket), "-data-dir", dataDir})
	if err == nil || !strings.Contains(err.Error(), "larger than one entry") {
		t.Fatalf("a large import: %v", err)
	}
	if cs := copyState(t, dataDir); cs.Import != apidb.ImportPending || cs.ImportOpID != "" {
		t.Fatalf("a refused import changed the copy's state: %+v", cs)
	}
}

// F1 of the 8c-1 review: an answer of users-read taken before the import
// was applied (no users) and applied after "done" deletes nothing and ends
// no session; the next answer applies normally and the sessions are kept.
func TestImportUsersStaleAnswerDeletesNothing(t *testing.T) {
	node := clustertest.Start(t)
	ctx := context.Background()
	dataDir := t.TempDir()
	oldHost(t, dataDir, "andy", "vera")
	addSession(t, dataDir, "00000000-0000-4000-8000-000000000001")
	addSession(t, dataDir, "00000000-0000-4000-8000-000000000002")
	client := userapi.NewClient(node.Socket)
	stale, err := client.UsersRead(ctx, 0, 0)
	if err != nil || stale.ClusterID != node.ClusterID || len(stale.Users) != 0 {
		t.Fatalf("the answer before the import: %+v %v", stale, err)
	}
	if out, err := runCmd(t, []string{"admin", "import-users", "-dev", "-config", configFor(t, node.Socket), "-data-dir", dataDir}); err != nil {
		t.Fatalf("import: %q %v", out, err)
	}
	db, err := database.Open(ctx, filepath.Join(dataDir, "api", "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := apidb.NewUserStore(db)
	cs, _ := store.CopyState(ctx)
	if cs.Import != apidb.ImportDone || cs.ClusterID != node.ClusterID {
		t.Fatalf("after the import: %+v", cs)
	}
	if res, err := store.ApplyAnswer(ctx, stale, time.Now()); err != nil || res.Changed {
		t.Fatalf("the stale answer: %+v %v", res, err)
	}
	if countRows(t, db, `SELECT COUNT(*) FROM users`) != 2 || countRows(t, db, `SELECT COUNT(*) FROM sessions`) != 2 {
		t.Fatal("the stale answer deleted users or sessions")
	}
	// The next answer (the import's) applies; the sessions are kept.
	next, err := client.UsersRead(ctx, 0, 0)
	if err != nil || len(next.Users) != 2 {
		t.Fatalf("the next answer: %+v %v", next, err)
	}
	db.Exec(`UPDATE cluster_copy SET users_index = ?`, int64(next.UsersIndex-1)) // as before the command's own sync
	if res, err := store.ApplyAnswer(ctx, next, time.Now()); err != nil || !res.Changed || res.SessionsEnded != 0 {
		t.Fatalf("the next answer: %+v %v", res, err)
	}
	if countRows(t, db, `SELECT COUNT(*) FROM users`) != 2 || countRows(t, db, `SELECT COUNT(*) FROM sessions`) != 2 {
		t.Fatal("the next answer lost users or sessions")
	}
}

// writeAnswerSocket answers every users-write with one error: a stand-in
// for skep-cluster in a given state, for the import's error paths.
func writeAnswerSocket(t *testing.T, status int, code string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "iu")
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
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(models.ErrorResponse{Error: models.ErrorBody{Code: code, Message: "test answer"}})
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return path
}

// V1 and F6c of the 8c-1 review: an unknown outcome is the no_quorum
// branch (errors.Is against the models.Error) and keeps the operation ID;
// any other error that is not one of D5's refusal codes keeps it too; a
// refusal (here "invalid") drops it.
func TestImportUsersKeepsTheOperationIDUnlessRefused(t *testing.T) {
	for _, c := range []struct {
		code, want string
		status     int
		kept       bool
	}{
		{"outcome_unknown", "no_quorum", http.StatusServiceUnavailable, true},
		{"internal_error", "is kept: run the command again", http.StatusInternalServerError, true},
		{"invalid", "was refused", http.StatusBadRequest, false},
	} {
		t.Run(c.code, func(t *testing.T) {
			dataDir := t.TempDir()
			oldHost(t, dataDir, "andy")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var out bytes.Buffer
			e := testEnv(&out, io.Discard)
			e.uid = 0
			err := run(ctx, []string{"admin", "import-users", "-dev", "-config", configFor(t, writeAnswerSocket(t, c.status, c.code)), "-data-dir", dataDir}, e)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%s: %v", c.code, err)
			}
			if c.code == "outcome_unknown" && (!errors.Is(err, &models.Error{Code: auth.CodeNoQuorum}) || !strings.Contains(err.Error(), "operation ID is recorded")) {
				t.Fatalf("an unknown outcome is not the no_quorum branch: %v", err)
			}
			cs := copyState(t, dataDir)
			if cs.Import != apidb.ImportPending || (cs.ImportOpID != "") != c.kept {
				t.Fatalf("%s: %+v", c.code, cs)
			}
		})
	}
}

// F5 and F6d of the 8c-1 review: "admin list" prints the users, with
// --import-state only the state word, and fails (on stderr, non-zero) when
// there is no api.db, so that the join does not say "no users".
func TestAdminListAndImportState(t *testing.T) {
	node := clustertest.Start(t)
	dataDir := t.TempDir()
	cfg := configFor(t, node.Socket)
	base := []string{"-dev", "-config", cfg, "-data-dir", dataDir}
	if out, err := runCmd(t, append([]string{"admin", "list"}, base...)); err == nil || !strings.Contains(err.Error(), "no api.db at") || out != "" {
		t.Fatalf("no api.db: %q %v", out, err)
	}
	if _, err := runCmd(t, append([]string{"admin", "list", "--import-state"}, base...)); err == nil {
		t.Fatal("no api.db: the import state was printed")
	}
	oldHost(t, dataDir, "andy", "vera")
	if out, err := runCmd(t, append([]string{"admin", "list"}, base...)); err != nil || out != "andy admin\nvera viewer\n" {
		t.Fatalf("list: %q %v", out, err)
	}
	if out, err := runCmd(t, append([]string{"admin", "list", "--import-state"}, base...)); err != nil || out != "pending\n" {
		t.Fatalf("import state: %q %v", out, err)
	}
	if _, err := runCmd(t, append([]string{"admin", "import-users"}, base...)); err != nil {
		t.Fatal(err)
	}
	if out, err := runCmd(t, append([]string{"admin", "list", "--import-state"}, base...)); err != nil || out != "done\n" {
		t.Fatalf("import state after the import: %q %v", out, err)
	}
}
