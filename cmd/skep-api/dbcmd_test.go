package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/database"
)

func TestDBBackupCommandRunsAsRoot(t *testing.T) {
	data := t.TempDir()
	db, err := database.Open(context.Background(), filepath.Join(data, "api", "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE TABLE t (x)`)
	db.Close()
	cfg := filepath.Join(t.TempDir(), "api.json")
	os.WriteFile(cfg, []byte("{}"), 0o600)
	var out, errb bytes.Buffer
	e := testEnv(&out, &errb)
	e.euid = 0 // the server refuses root; db backup does not
	if err := run(context.Background(), []string{"db", "backup", "-config", cfg, "-data-dir", data}, e); err != nil {
		t.Fatalf("%v: %s", err, errb.String())
	}
	if !strings.Contains(out.String(), "api.db sha256") {
		t.Fatalf("output %q", out.String())
	}
}
