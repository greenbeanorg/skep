package auditcmd_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/auditarchive"
	"github.com/greenbeanorg/skep/internal/auditcmd"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/migrations"
)

// A sealed month and one current entry, with the writer still open as a
// running agent's would be.
func TestExportUnsealedWhileInUse(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent", "agent.db")
	w, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := database.Migrate(ctx, w, migrations.Agent()); err != nil {
		t.Fatal(err)
	}
	insert := func(at, action string) {
		t.Helper()
		if _, err := w.Exec(`INSERT INTO audit_log (occurred_at, actor_id, actor_name, peer_uid, action, outcome, details)
			VALUES (?, 'system', 'system', 0, ?, 'success', '{"a": 1}')`, at, action); err != nil {
			t.Fatal(err)
		}
	}
	insert("2020-03-10T12:00:00.000Z", "vm.start")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	archiveDir := filepath.Join(dir, "agent", "audit-archive")
	sealer, err := auditarchive.New(auditarchive.Options{DB: w, Log: auditarchive.AgentLog, Dir: archiveDir, Logger: logger,
		NodeID: func(context.Context) (string, error) { return "node-1", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if s, err := sealer.Seal(ctx); err != nil || len(s) != 1 {
		t.Fatalf("Seal = %+v, %v", s, err)
	}
	insert("2099-01-01T00:00:00.000Z", "container.sweep.stop_completed")
	sum := func() string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%x", sha256.Sum256(b))
	}
	before := sum()

	r, err := auditcmd.OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Exec(`INSERT INTO audit_log (actor_id, actor_name, peer_uid, action, outcome) VALUES ('x','x',0,'x','success')`); err == nil {
		t.Fatal("the read-only connection could write")
	}
	a, err := auditarchive.New(auditarchive.Options{DB: r, Log: auditarchive.AgentLog, Dir: archiveDir, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := auditcmd.ExportUnsealed(ctx, a, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Count(got, "\n") != 1 || !strings.HasPrefix(got, `{"id":2,"occurred_at":"2099-01-01T00:00:00.000Z",`) ||
		!strings.Contains(got, `"action":"container.sweep.stop_completed"`) || !strings.HasSuffix(got, `"details":{"a":1}}`+"\n") {
		t.Fatalf("exported:\n%s", got)
	}
	if sum() != before {
		t.Error("the database file changed")
	}
	if segs, err := a.Segments(ctx); err != nil || len(segs) != 1 {
		t.Errorf("segments after export = %d, %v", len(segs), err)
	}
}

// A database nobody has open is refused, and no file appears next to it.
func TestOpenReadOnlyRefusesAndCreatesNothing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent", "agent.db")
	w, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Migrate(ctx, w, migrations.Agent()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := auditcmd.OpenReadOnly(ctx, path); !errors.Is(err, database.ErrNotInUse) {
		t.Fatalf("OpenReadOnly = %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("files after the refusal: %v", entries)
	}
	if _, err := auditcmd.OpenReadOnly(ctx, filepath.Join(dir, "absent.db")); err == nil || !strings.Contains(err.Error(), "no database") {
		t.Errorf("missing database: %v", err)
	}
}
