package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/greenbeanorg/skep/internal/auditarchive"
	"github.com/greenbeanorg/skep/internal/auditcmd"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/migrations"
)

// sealedDataDir returns a data directory whose agent database holds audit
// entries of two long-past months, both sealed, and a current one. No
// agent is running on it.
func sealedDataDir(t *testing.T) (dataDir string, files []string) {
	t.Helper()
	ctx := context.Background()
	dataDir = t.TempDir()
	db, err := database.Open(ctx, filepath.Join(dataDir, "agent", "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := database.Migrate(ctx, db, migrations.Agent()); err != nil {
		t.Fatal(err)
	}
	for i, at := range []string{
		"2020-03-10T12:00:00.000Z", "2020-03-11T12:00:00.000Z", "2020-03-12T12:00:00.000Z",
		"2020-04-10T12:00:00.000Z", "2020-04-11T12:00:00.000Z",
	} {
		if _, err := db.Exec(`INSERT INTO audit_log (occurred_at, actor_id, actor_name, peer_uid, action, outcome, details)
			VALUES (?, 'u1', 'andy', 1001, 'vm.start', 'success', ?)`, at, fmt.Sprintf(`{"n":%d}`, i+1)); err != nil {
			t.Fatal(err)
		}
	}
	// Dated now: in a month that has not ended, so never sealed here.
	if _, err := db.Exec(`INSERT INTO audit_log (actor_id, actor_name, peer_uid, action, outcome) VALUES ('u1', 'andy', 1001, 'vm.stop', 'success')`); err != nil {
		t.Fatal(err)
	}
	archive, err := auditarchive.New(auditarchive.Options{
		DB: db, Log: auditarchive.AgentLog, Dir: filepath.Join(dataDir, "agent", "audit-archive"),
		NodeID: func(context.Context) (string, error) { return "node-1", nil },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := archive.Seal(ctx)
	if err != nil || len(sealed) != 2 {
		t.Fatalf("Seal = %+v, %v", sealed, err)
	}
	for _, s := range sealed {
		files = append(files, filepath.Join(archive.Dir(), s.FileName))
	}
	return dataDir, files
}

func auditCommand(t *testing.T, dataDir string, args ...string) (stdout string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	full := append([]string{"audit"}, args...)
	if len(args) > 0 {
		full = append(full, "-dev", "-data-dir", dataDir)
	}
	err = run(context.Background(), full, &out, &errOut)
	return out.String(), err
}

func TestAuditVerifyCommand(t *testing.T) {
	dataDir, files := sealedDataDir(t)

	out, err := auditCommand(t, dataDir, "verify")
	if err != nil {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	for _, want := range []string{
		"ok       2020-03  entries 1-3 (3)  audit-node-1-2020-03-1-3.jsonl.gz",
		"ok       2020-04  entries 4-5 (2)  audit-node-1-2020-04-4-5.jsonl.gz",
		"2 sealed segments in " + filepath.Join(dataDir, "agent", "audit-archive") + ": 2 ok, 0 missing, 0 invalid",
		"head of the chain: segment 2 (2020-04) sha256 ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("verify printed:\n%s\nwant a line with %q", out, want)
		}
	}

	// A segment moved to other storage: always reported, accepted only
	// when told to.
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(files[0], moved); err != nil {
		t.Fatal(err)
	}
	out, err = auditCommand(t, dataDir, "verify")
	if !errors.Is(err, auditarchive.ErrMissing) || !strings.Contains(out, "missing  2020-03") || !strings.Contains(out, "1 ok, 1 missing, 0 invalid") {
		t.Errorf("verify with a moved segment: %v\n%s", err, out)
	}
	out, err = auditCommand(t, dataDir, "verify", "-allow-missing")
	if err != nil || !strings.Contains(out, "missing  2020-03") {
		t.Errorf("verify -allow-missing: %v\n%s", err, out)
	}
	if err := os.Rename(moved, files[0]); err != nil {
		t.Fatal(err)
	}

	// An altered file is the first break, with or without --allow-missing.
	if err := os.Chmod(files[1], 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(files[1])
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(files[1], data, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = auditCommand(t, dataDir, "verify", "-allow-missing")
	if !errors.Is(err, auditarchive.ErrInvalid) || !strings.Contains(err.Error(), "segment 2") || !strings.Contains(out, "invalid  2020-04") {
		t.Errorf("verify of an altered segment: %v\n%s", err, out)
	}

	// A stray file is reported and left alone.
	stray := filepath.Join(filepath.Dir(files[0]), "audit-node-1-2020-05-6-9.jsonl.gz")
	if err := os.WriteFile(stray, []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	out, _ = auditCommand(t, dataDir, "verify")
	if !strings.Contains(out, "stray    audit-node-1-2020-05-6-9.jsonl.gz") {
		t.Errorf("verify printed:\n%s\nwant the stray file reported", out)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("the stray file was touched: %v", err)
	}
}

func TestAuditExportCommand(t *testing.T) {
	dataDir, files := sealedDataDir(t)

	out, err := auditCommand(t, dataDir, "export")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("export printed %d lines, want the 5 archived entries:\n%s", len(lines), out)
	}
	for i, line := range lines {
		var e struct {
			ID         int64  `json:"id"`
			OccurredAt string `json:"occurred_at"`
			Action     string `json:"action"`
			PeerUID    int    `json:"peer_uid"`
			Details    struct {
				N int `json:"n"`
			} `json:"details"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d is not JSON: %v\n%s", i+1, err, line)
		}
		if e.ID != int64(i+1) || e.Action != "vm.start" || e.PeerUID != 1001 || e.Details.N != i+1 || !strings.HasPrefix(e.OccurredAt, "2020-0") {
			t.Errorf("line %d = %s", i+1, line)
		}
	}

	out, err = auditCommand(t, dataDir, "export", "-month", "2020-04")
	if err != nil || strings.Count(out, "\n") != 2 || !strings.HasPrefix(out, `{"id":4,`) {
		t.Errorf("export -month 2020-04: %v\n%s", err, out)
	}
	if _, err := auditCommand(t, dataDir, "export", "-month", "2020-05"); err == nil || !strings.Contains(err.Error(), "no sealed segment") {
		t.Errorf("export of a month without a segment: %v", err)
	}
	for _, bad := range []string{"2020-4", "2020-13", "april", "2020-04-01"} {
		if _, err := auditCommand(t, dataDir, "export", "-month", bad); err == nil || !strings.Contains(err.Error(), "YYYY-MM") {
			t.Errorf("export -month %s: %v", bad, err)
		}
	}

	// Nothing of a segment that does not verify is printed.
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	out, err = auditCommand(t, dataDir, "export")
	if !errors.Is(err, auditarchive.ErrMissing) || out != "" {
		t.Errorf("export with a missing segment: %v, printed %q", err, out)
	}
}

func TestAuditCommandErrors(t *testing.T) {
	// No database: an error, and the command creates nothing.
	empty := t.TempDir()
	if _, err := auditCommand(t, empty, "verify"); err == nil || !strings.Contains(err.Error(), "no database") {
		t.Errorf("verify without a database: %v", err)
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Errorf("verify created %v", entries)
	}

	// A database from before retention: reported as such, and not migrated
	// by a command that promises to change nothing.
	ctx := context.Background()
	old := t.TempDir()
	db, err := database.Open(ctx, filepath.Join(old, "agent", "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	all := migrations.Agent()
	before := fstest.MapFS{}
	for _, name := range []string{"0001_initial_schema.sql", "0002_task_engine.sql"} {
		body, err := fs.ReadFile(all, name)
		if err != nil {
			t.Fatal(err)
		}
		before[name] = &fstest.MapFile{Data: body}
	}
	if _, err := database.Migrate(ctx, db, before); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := auditCommand(t, old, "export"); !errors.Is(err, auditcmd.ErrNoIndex) {
		t.Errorf("export on a schema 2 database: %v", err)
	}
	db, err = database.Open(ctx, filepath.Join(old, "agent", "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v, err := database.SchemaVersion(ctx, db); err != nil || v != 2 {
		t.Errorf("schema version after the command = %d, %v; want it untouched at 2", v, err)
	}

	for _, args := range [][]string{{}, {"seal"}, {"remove"}, {"verify", "extra"}} {
		if _, err := auditCommand(t, empty, args...); err == nil {
			t.Errorf("audit %v was accepted", args)
		}
	}
}
