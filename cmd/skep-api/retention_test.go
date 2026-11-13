package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/apidb"
	"github.com/greenbeanorg/skep/internal/auditarchive"
	"github.com/greenbeanorg/skep/internal/config"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/migrations"
	"github.com/greenbeanorg/skep/pkg/models"
)

// seedAccessLog creates api.db in dataDir with access audit entries at the
// given timestamps and returns the open database.
func seedAccessLog(t *testing.T, dataDir string, stamps ...string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(dataDir, "api", "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := database.Migrate(ctx, db, migrations.API()); err != nil {
		t.Fatal(err)
	}
	for _, at := range stamps {
		if _, err := db.Exec(`INSERT INTO audit_log (occurred_at, actor_name, action, outcome, source_ip, details)
			VALUES (?, 'mallory', 'auth.login', 'denied', '192.0.2.7', '{"reason":"bad_credentials"}')`, at); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// stubAgent answers the one question the archive asks the agent.
type stubAgent struct {
	down   atomic.Bool
	caller atomic.Value
}

func (s *stubAgent) System(ctx context.Context) (models.System, error) {
	if c, ok := agentproto.CallerFrom(ctx); ok {
		s.caller.Store(c.ActorID)
	}
	if s.down.Load() {
		return models.System{}, errors.New("the agent is not reachable")
	}
	return models.System{NodeID: "node-1"}, nil
}

type archiveEntry struct {
	Outcome string
	Details map[string]any
}

// archiveEntries returns the audit.archive entries, oldest first.
func archiveEntries(t *testing.T, db *sql.DB) []archiveEntry {
	t.Helper()
	rows, err := db.Query(`SELECT actor_name, outcome, details FROM audit_log WHERE action = 'audit.archive' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []archiveEntry
	for rows.Next() {
		var actor, raw string
		var e archiveEntry
		if err := rows.Scan(&actor, &e.Outcome, &raw); err != nil {
			t.Fatal(err)
		}
		if actor != "system" {
			t.Errorf("audit.archive actor = %q", actor)
		}
		if err := json.Unmarshal([]byte(raw), &e.Details); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// TestAccessArchiveRuns drives the access log's archive run with an
// injected clock. Entries are dated in 2020, so the run's own audit
// entries, which are stamped with the real time, never fall into a month
// the test seals.
func TestAccessArchiveRuns(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db := seedAccessLog(t, dataDir,
		"2020-09-10T12:00:00.000Z", "2020-09-11T12:00:00.000Z", "2020-09-30T23:59:59.999Z", // 1-3
		"2020-10-01T00:00:00.000Z", "2020-10-10T12:00:00.000Z") // 4-5
	dev := true
	cfg, err := config.LoadAPI(config.APILoadOptions{DefaultPath: "/nonexistent", Overrides: config.APIOverrides{DevMode: &dev, DataDir: dataDir},
		LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Retention.AuditLiveDaysOrZero() != 90 {
		t.Fatalf("default live days = %d, want 90", cfg.Retention.AuditLiveDaysOrZero())
	}

	var clock atomic.Int64
	set := func(s string) {
		at, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		clock.Store(at.UnixNano())
	}
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	agent := &stubAgent{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	access := apidb.NewAccessLog(db)
	archive, err := newAccessArchive(db, cfg, agent, log, now)
	if err != nil || archive == nil {
		t.Fatal(archive, err)
	}
	run := func() error { return archiveOnce(ctx, archive, cfg.Retention.AuditLiveDaysOrZero(), access, log) }
	live := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE id <= 5`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// September has ended, but the agent is down and no segment exists
	// yet: the node ID is unknown, nothing is sealed, and the failed run
	// is on record.
	set("2020-10-05T00:00:00Z")
	agent.down.Store(true)
	if err := run(); err == nil {
		t.Fatal("the run succeeded without a node ID")
	}
	entries := archiveEntries(t, db)
	if len(entries) != 1 || entries[0].Outcome != "failure" || entries[0].Details["error_code"] != "audit_archive_failed" {
		t.Fatalf("entries = %+v", entries)
	}
	if segs, _ := archive.Segments(ctx); len(segs) != 0 {
		t.Fatalf("segments = %+v", segs)
	}

	// The agent is back. skep-api asks it as itself, not as a user.
	agent.down.Store(false)
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if got := agent.caller.Load(); got != agentproto.APIActor {
		t.Errorf("the agent was asked as %v, want %q", got, agentproto.APIActor)
	}
	entries = archiveEntries(t, db)
	sealed, _ := entries[1].Details["segments_sealed"].([]any)
	if len(entries) != 2 || entries[1].Outcome != "success" || len(sealed) != 1 || entries[1].Details["entries_removed"] != float64(0) ||
		entries[1].Details["live_days"] != float64(90) {
		t.Fatalf("entries = %+v", entries)
	}
	if seg := sealed[0].(map[string]any); seg["month"] != "2020-09" || seg["first_id"] != float64(1) || seg["last_id"] != float64(3) {
		t.Errorf("sealed = %v", seg)
	}
	// The archive is the service account's own: 0700, files 0400.
	dir := filepath.Join(dataDir, "api", "audit-archive")
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("archive directory: %v, %v", info, err)
	}
	file := filepath.Join(dir, "audit-node-1-2020-09-1-3.jsonl.gz")
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o400 {
		t.Errorf("segment file: %v, %v", info, err)
	}

	// October is sealed while the agent is down again: the node ID comes
	// from the previous segment.
	set("2020-11-01T00:00:00Z")
	agent.down.Store(true)
	if err := run(); err != nil {
		t.Fatalf("sealing with the agent down and an earlier segment: %v", err)
	}
	segs, _ := archive.Segments(ctx)
	if len(segs) != 2 || segs[1].NodeID != "node-1" || segs[1].Month != "2020-10" || segs[1].FirstID != 4 || segs[1].LastID != 5 {
		t.Fatalf("segments = %+v", segs)
	}
	if live() != 5 {
		t.Fatalf("%d live entries; sealing must remove nothing", live())
	}

	// 90 days after September ended (October 1st) is December 30th.
	set("2020-12-29T23:59:59Z")
	if err := run(); err != nil || live() != 5 {
		t.Fatalf("one second early: %v, %d live", err, live())
	}
	set("2020-12-30T00:00:00Z")
	if err := run(); err != nil || live() != 2 {
		t.Fatalf("on the day: %v, %d live; want September's 3 entries removed", err, live())
	}
	entries = archiveEntries(t, db)
	last := entries[len(entries)-1]
	if last.Details["entries_removed"] != float64(3) || len(last.Details["months_removed"].([]any)) != 1 {
		t.Errorf("entry of the removing run = %+v", last)
	}

	// October's file is altered before it is due: nothing more is removed
	// and the failure is on record with a specific code.
	october := filepath.Join(dir, segs[1].FileName)
	if err := os.Chmod(october, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(october, []byte("gone"), 0o600); err != nil {
		t.Fatal(err)
	}
	set("2021-03-01T00:00:00Z")
	if err := run(); !errors.Is(err, auditarchive.ErrInvalid) || live() != 2 {
		t.Fatalf("with an altered segment: %v, %d live", err, live())
	}
	entries = archiveEntries(t, db)
	last = entries[len(entries)-1]
	if last.Outcome != "failure" || last.Details["error_code"] != "audit_archive_invalid" || last.Details["entries_removed"] != float64(0) {
		t.Errorf("entry of the failed run = %+v", last)
	}
	// The entry does not leak file names or paths to API readers.
	if raw, _ := json.Marshal(last.Details); strings.Contains(string(raw), dataDir) {
		t.Errorf("the entry names a path: %s", raw)
	}
}

func TestAccessArchiveCanBeTurnedOff(t *testing.T) {
	dataDir := t.TempDir()
	db := seedAccessLog(t, dataDir, "2020-09-10T12:00:00.000Z")
	path := filepath.Join(t.TempDir(), "api.json")
	if err := os.WriteFile(path, []byte(`{"retention": {"audit_archive": false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dev := true
	cfg, err := config.LoadAPI(config.APILoadOptions{Path: path, Overrides: config.APIOverrides{DevMode: &dev, DataDir: dataDir},
		LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := newAccessArchive(db, cfg, &stubAgent{}, slog.Default(), time.Now)
	if err != nil || archive != nil {
		t.Errorf("archive = %v, %v; want none", archive, err)
	}
}

// TestServerArchivesItsAccessLog starts the real server on a database with
// old entries: at start it seals them and, because they are far older than
// 90 days, removes them from the live table. The audit commands then read
// them back.
func TestServerArchivesItsAccessLog(t *testing.T) {
	dir, err := os.MkdirTemp("", "skep-api")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "agent.sock")
	dataDir, cfg := filepath.Join(dir, "data"), fastConfig(t)
	seed := seedAccessLog(t, dataDir,
		"2020-03-10T12:00:00.000Z", "2020-03-11T12:00:00.000Z", "2020-04-10T12:00:00.000Z")
	seed.Close()
	defer startAgent(t, socket, &fakeAgent{})()
	_, logs := startAPIWithLogs(t, "-dev", "-config", cfg, "-data-dir", dataDir, "-agent-socket", socket)

	db := openAPIDB(t, dataDir)
	deadline := time.Now().Add(15 * time.Second)
	for countRows(t, db, `SELECT COUNT(*) FROM audit_log WHERE action = 'audit.archive'`) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no archive run was recorded:\n%s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM audit_segments`); n != 2 {
		t.Errorf("%d segments, want March and April 2020", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM audit_log WHERE id <= 3`); n != 0 {
		t.Errorf("%d of the old entries are still live", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM audit_log WHERE action = 'audit.archive' AND outcome = 'success'`); n != 1 {
		t.Errorf("%d successful audit.archive entries, want 1", n)
	}
	logs.stop()

	// The commands work with the server stopped.
	var out bytes.Buffer
	if err := run(context.Background(), []string{"audit", "verify", "-dev", "-data-dir", dataDir}, testEnv(&out, io.Discard)); err != nil {
		t.Fatalf("audit verify: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "2 ok, 0 missing, 0 invalid") || !strings.Contains(out.String(), "audit-node-1-2020-03-1-2.jsonl.gz") {
		t.Errorf("audit verify printed:\n%s", out.String())
	}
	out.Reset()
	if err := run(context.Background(), []string{"audit", "export", "-dev", "-data-dir", dataDir}, testEnv(&out, io.Discard)); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "\n") != 3 || !strings.Contains(out.String(), `"actor_name":"mallory"`) ||
		!strings.Contains(out.String(), `"details":{"reason":"bad_credentials"}`) {
		t.Errorf("audit export printed:\n%s", out.String())
	}
	for _, args := range [][]string{{"audit"}, {"audit", "seal"}} {
		if err := run(context.Background(), args, testEnv(io.Discard, io.Discard)); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}
