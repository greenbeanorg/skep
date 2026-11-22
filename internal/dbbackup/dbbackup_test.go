package dbbackup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/database"
)

// liveDB makes a WAL database with rows, as the services leave it, and
// keeps it open (a running service).
func liveDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent", "agent.db")
	db, err := database.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE tasks (id TEXT PRIMARY KEY, status TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, err := db.Exec(`INSERT INTO tasks VALUES (?, 'succeeded')`, id); err != nil {
			t.Fatal(err)
		}
	}
	return path, db
}

func clock(start time.Time) func() time.Time {
	t := start
	return func() time.Time { t = t.Add(time.Minute); return t }
}

func TestBackupCopiesLiveDatabase(t *testing.T) {
	path, live := liveDB(t)
	archive := t.TempDir()
	os.WriteFile(filepath.Join(archive, "agent-2026-10.jsonl"), []byte("sealed\n"), 0o400)
	parent := filepath.Join(t.TempDir(), "backup")
	r, err := Backup(context.Background(), Options{Name: "agent", DBPath: path, ArchiveDir: archive, Parent: parent,
		Label: "0.1.0-dd7a8b6", Keep: 3, Now: clock(time.Date(2026, 11, 22, 16, 0, 0, 0, time.UTC))})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(r.Dir) != "0.1.0-dd7a8b6-20261122T160100Z" {
		t.Errorf("dir %s", r.Dir)
	}
	copyDB, err := sql.Open("sqlite", readOnlyDSN(r.File))
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var n int
	if err := copyDB.QueryRow(`SELECT count(*) FROM tasks`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("copy has %d rows, %v", n, err)
	}
	for p, mode := range map[string]os.FileMode{r.Dir: 0o700, r.File: 0o600, r.File + ".sha256": 0o600} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != mode {
			t.Errorf("%s: %v %v", p, st, err)
		}
	}
	sumLine, _ := os.ReadFile(r.File + ".sha256")
	if !strings.HasPrefix(string(sumLine), r.SHA256+"  agent.db") {
		t.Errorf("sha256 file %q", sumLine)
	}
	listing, _ := os.ReadFile(r.Archive)
	if !strings.Contains(string(listing), "7  agent-2026-10.jsonl") || !strings.Contains(string(listing), "not copied") {
		t.Errorf("archive listing %q", listing)
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "agent-2026-10.jsonl")); err == nil {
		t.Error("the archive was copied")
	}
	// The live database still works and was not written by the backup.
	if _, err := live.Exec(`INSERT INTO tasks VALUES ('d', 'running')`); err != nil {
		t.Fatal(err)
	}
}

func TestBackupBothIntoOneDirectoryAndKeepThree(t *testing.T) {
	path, _ := liveDB(t)
	apiPath, _ := liveDB(t)
	parent := filepath.Join(t.TempDir(), "backup")
	now := clock(time.Date(2026, 11, 22, 16, 0, 0, 0, time.UTC))
	var dirs []string
	for i := 0; i < 5; i++ {
		dir := filepath.Join(parent, "0.1.0-"+now().UTC().Format("20060102T150405Z"))
		if _, err := Backup(context.Background(), Options{Name: "agent", DBPath: path, Parent: parent, Dir: dir, Keep: 3}); err != nil {
			t.Fatal(err)
		}
		if _, err := Backup(context.Background(), Options{Name: "api", DBPath: apiPath, Parent: parent, Dir: dir, Keep: 3}); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, dir)
	}
	// Something else in the backup directory is never removed.
	os.MkdirAll(filepath.Join(parent, "keepme-20200101T000000Z"), 0o700)
	if _, err := prune(parent, 3); err != nil {
		t.Fatal(err)
	}
	for i, d := range dirs {
		_, err := os.Stat(d)
		if (i < 2) != os.IsNotExist(err) {
			t.Errorf("backup %d (%s): exists=%v", i, filepath.Base(d), err == nil)
		}
		if i >= 2 {
			for _, f := range []string{"agent.db", "api.db"} {
				if _, err := os.Stat(filepath.Join(d, f)); err != nil {
					t.Errorf("%s lacks %s", d, f)
				}
			}
		}
	}
	if _, err := os.Stat(filepath.Join(parent, "keepme-20200101T000000Z")); err != nil {
		t.Error("removed a directory without the marker")
	}
}

func TestBackupRefusals(t *testing.T) {
	path, _ := liveDB(t)
	parent := filepath.Join(t.TempDir(), "backup")
	base := Options{Name: "agent", DBPath: path, Parent: parent, Label: "0.1.0", Keep: 3}
	cases := map[string]func(o *Options){
		"bad name":         func(o *Options) { o.Name = "x" },
		"keep 0":           func(o *Options) { o.Keep = 0 },
		"missing database": func(o *Options) { o.DBPath += ".nope" },
		"bad label":        func(o *Options) { o.Label = "../x" },
		"dir elsewhere":    func(o *Options) { o.Dir = filepath.Join(t.TempDir(), "0.1.0-20261122T160000Z") },
		"dir badly named":  func(o *Options) { o.Dir = filepath.Join(parent, "mine") },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			o := base
			mod(&o)
			if _, err := Backup(context.Background(), o); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	t.Run("never overwrites", func(t *testing.T) {
		o := base
		o.Dir = filepath.Join(parent, "0.1.0-20261122T170000Z")
		if _, err := Backup(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		if _, err := Backup(context.Background(), o); err == nil || !strings.Contains(err.Error(), "never overwritten") {
			t.Fatalf("err %v", err)
		}
	})
}
