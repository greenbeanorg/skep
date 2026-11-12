package database

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/migrations"
)

func openAPIDB(t *testing.T) *AccessLog {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "api", "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := Migrate(ctx, db, migrations.API()); err != nil {
		t.Fatal(err)
	}
	return NewAccessLog(db)
}

func TestAccessLogWrite(t *testing.T) {
	ctx := context.Background()
	log := openAPIDB(t)

	if err := log.Write(ctx, audit.AccessEntry{
		ActorName: "local:0", Action: "tls.key.regenerate", ResourceType: "tls", Outcome: audit.Success,
		Details: map[string]any{"new_public_key_sha256": "sha256:abc"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := log.Write(ctx, audit.AccessEntry{
		ActorID: "user-1", ActorName: "andy", Action: "auth.login", Outcome: audit.Failure,
		SourceIP: "192.0.2.7", RequestID: "req-1",
	}); err != nil {
		t.Fatal(err)
	}

	var actorID *string
	var actor, action, outcome, details string
	if err := log.db.QueryRowContext(ctx, `SELECT actor_id, actor_name, action, outcome, details FROM audit_log ORDER BY id LIMIT 1`).
		Scan(&actorID, &actor, &action, &outcome, &details); err != nil {
		t.Fatal(err)
	}
	if actorID != nil || actor != "local:0" || action != "tls.key.regenerate" || outcome != "success" ||
		!strings.Contains(details, "sha256:abc") {
		t.Errorf("row = %v %q %q %q %q", actorID, actor, action, outcome, details)
	}

	for name, e := range map[string]audit.AccessEntry{
		"no actor":    {Action: "x", Outcome: audit.Success},
		"no action":   {ActorName: "andy", Outcome: audit.Success},
		"bad outcome": {ActorName: "andy", Action: "x", Outcome: "maybe"},
	} {
		if err := log.Write(ctx, e); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// The log is append-only, whoever asks.
	if _, err := log.db.ExecContext(ctx, `DELETE FROM audit_log`); err == nil {
		t.Error("audit entries could be deleted")
	}
	if _, err := log.db.ExecContext(ctx, `UPDATE audit_log SET actor_name = 'x'`); err == nil {
		t.Error("audit entries could be changed")
	}
}
