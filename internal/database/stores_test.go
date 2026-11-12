package database

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/pkg/models"
)

func TestAuditLogWrite(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t)
	log := NewAuditLog(db)

	entry := audit.Entry{
		ActorID: "u1", ActorName: "andy", PeerUID: 1001,
		Action: "vm.start", ResourceType: "vm", ResourceID: "vm-1",
		Phase: audit.Requested, Outcome: audit.Denied,
		RequestID: "req-1", Details: map[string]any{"error_code": "resource_busy"},
	}
	if err := log.Write(ctx, entry); err != nil {
		t.Fatal(err)
	}
	var (
		actor, action, phase, outcome, details string
		peer                                   int
		taskID                                 *string
	)
	if err := db.QueryRow(`SELECT actor_id, peer_uid, action, phase, outcome, details, task_id FROM audit_log`).
		Scan(&actor, &peer, &action, &phase, &outcome, &details, &taskID); err != nil {
		t.Fatal(err)
	}
	if actor != "u1" || peer != 1001 || action != "vm.start" || phase != "requested" || outcome != "denied" || taskID != nil {
		t.Errorf("row = %s %d %s %s %s %v", actor, peer, action, phase, outcome, taskID)
	}
	var d map[string]string
	if err := json.Unmarshal([]byte(details), &d); err != nil || d["error_code"] != "resource_busy" {
		t.Errorf("details = %s", details)
	}

	// An entry without an actor is refused before it reaches the database.
	entry.ActorID = ""
	if err := log.Write(ctx, entry); !errors.Is(err, audit.ErrActorRequired) {
		t.Errorf("error = %v, want ErrActorRequired", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&n); err != nil || n != 1 {
		t.Errorf("audit rows = %d, %v; want 1", n, err)
	}
}

func TestNodeStoreSaveObserved(t *testing.T) {
	ctx := context.Background()
	db := migratedDB(t)
	id, err := EnsureLocalNode(ctx, db, "hearst")
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `UPDATE nodes SET desired_state = 'maintenance' WHERE id = ?`, id)

	collected := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	hw := models.Hardware{
		NodeID:   id,
		Platform: models.PlatformFreeBSD,
		Host: models.HostInfo{
			Hostname:     "hearst.greenbean.org",
			OS:           models.OSInfo{Name: "FreeBSD", Release: "15.1-RELEASE-p6"},
			Architecture: "amd64",
		},
		CPU:         models.CPUInfo{Model: "Test CPU", Cores: 2, Threads: 2},
		CollectedAt: collected,
	}
	store := NewNodeStore(db)
	if err := store.SaveObserved(ctx, id, hw); err != nil {
		t.Fatal(err)
	}

	var state, desired, hostname, osName, release, arch, doc, at string
	if err := db.QueryRow(`SELECT observed_state, desired_state, observed_hostname, observed_os_name,
		observed_os_release, observed_arch, observed_hardware, observed_at FROM nodes WHERE id = ?`, id).
		Scan(&state, &desired, &hostname, &osName, &release, &arch, &doc, &at); err != nil {
		t.Fatal(err)
	}
	if state != "online" || hostname != "hearst.greenbean.org" || osName != "FreeBSD" ||
		release != "15.1-RELEASE-p6" || arch != "amd64" || at != "2026-11-12T09:30:00.000Z" {
		t.Errorf("observed = %s %s %s %s %s %s", state, hostname, osName, release, arch, at)
	}
	// Observation never overwrites desired state.
	if desired != "maintenance" {
		t.Errorf("desired_state = %q; saving observed state must not change it", desired)
	}
	var back models.Hardware
	if err := json.Unmarshal([]byte(doc), &back); err != nil || back.CPU.Model != "Test CPU" {
		t.Errorf("stored hardware = %s (%v)", doc, err)
	}

	if err := store.SaveObserved(ctx, "no-such-node", hw); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}
