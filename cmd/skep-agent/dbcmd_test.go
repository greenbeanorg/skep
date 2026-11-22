package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/dbbackup"
	"github.com/greenbeanorg/skep/pkg/version"
)

func TestDBBackupCommand(t *testing.T) {
	data := t.TempDir()
	db, err := database.Open(context.Background(), filepath.Join(data, "agent", "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE TABLE t (x)`)
	db.Close()
	t.Setenv("SKEP_CONFIG", "")
	var out, errb bytes.Buffer
	cfg := filepath.Join(t.TempDir(), "agent.json")
	os.WriteFile(cfg, []byte("{}"), 0o600)
	for i := 0; i < 2; i++ {
		out.Reset()
		if err := run(context.Background(), []string{"db", "backup", "-config", cfg, "-data-dir", data, "-keep", "1"}, &out, &errb); err != nil {
			t.Fatalf("%v: %s", err, errb.String())
		}
		if i == 0 {
			// One label format, <version>+<commit>, as upgrade's.
			if !strings.Contains(out.String(), "/backup/"+dbbackup.Label(version.Get().Version, version.Get().Commit)+"-") {
				t.Fatalf("label: %q", out.String())
			}
			// the next backup needs another timestamp
			os.Rename(filepath.Dir(strings.Fields(out.String())[3]), filepath.Join(data, "backup", "0.1.0-dev-unknown-20200101T000000Z"))
		}
	}
	if !strings.Contains(out.String(), "agent.db sha256") || !strings.Contains(out.String(), "removed the old backup") {
		t.Fatalf("output %q", out.String())
	}
	entries, _ := os.ReadDir(filepath.Join(data, "backup"))
	if len(entries) != 1 {
		t.Fatalf("%d backups kept, want 1", len(entries))
	}
	if err := run(context.Background(), []string{"db"}, &out, &errb); err == nil {
		t.Error("db without a command accepted")
	}
	if err := run(context.Background(), []string{"db", "restore"}, &out, &errb); err == nil {
		t.Error("unknown db command accepted")
	}
}
