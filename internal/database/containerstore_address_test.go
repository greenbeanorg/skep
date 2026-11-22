package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/migrations"
	"github.com/greenbeanorg/skep/pkg/models"
)

// TestMarkStartingRefusesHeldAddress is SR-07 at the store: MarkStarting
// refuses, in its own transaction, a jail whose static address a jail
// recorded starting or running holds on the same bridge.
func TestMarkStartingRefusesHeldAddress(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db, migrations.Agent()); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO nodes (id, name, is_local) VALUES ('n1', 'node1', 1)`)
	at := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	tpls := NewTemplateStore(db)
	tpl := models.Template{ID: "3f2a9c1e-7b4d-4e8f-9a01-23456789abcd", Name: "minimal-151", Pool: "skeptest",
		Source: models.TemplateSourcePkgbase, Packages: []string{"FreeBSD-set-minimal"}, CreatedAt: at}
	tx, _ := db.BeginTx(ctx, nil)
	if err := tpls.InsertTx(ctx, tx, "n1", tpl); err != nil {
		t.Fatal(err)
	}
	tx.Commit()

	s := NewContainerStore(db)
	n := 0
	jail := func(name, bridge, addr, mode string) models.Container {
		n++
		id := []string{"", "a", "b", "c", "d", "e"}[n]
		c := models.Container{ID: id + "1a1a1a1-1111-4222-8333-444455556666", Name: name, Hostname: name, TemplateID: tpl.ID, CreatedAt: at,
			NICs: []models.ContainerNIC{{ID: id + "2a2a2a2-1111-4222-8333-444455556666", Slot: 0, Bridge: bridge,
				MACAddress: "02:00:00:00:00:0" + id, IPv4: models.ContainerIPv4{Mode: mode, Address: addr}}}}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := s.InsertTx(ctx, tx, "n1", c); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := s.SetProvisioned(ctx, c.ID, at); err != nil {
			t.Fatal(err)
		}
		return c
	}
	a := jail("a", "bridge0", "192.0.2.10/24", models.IPv4Static)
	b := jail("b", "bridge0", "192.0.2.10/25", models.IPv4Static) // same IP, another prefix
	other := jail("other", "bridge1", "192.0.2.10/24", models.IPv4Static)
	dhcp := jail("dhcp", "bridge0", "", models.IPv4DHCP)
	c := jail("c", "bridge0", "192.0.2.11/24", models.IPv4Static)

	// b is stopped while a starts: nothing holds the address.
	if err := s.MarkStarting(ctx, a.ID, at); err != nil {
		t.Fatal(err)
	}
	refused := func(id, holder string) {
		t.Helper()
		err := s.MarkStarting(ctx, id, at)
		var me *models.Error
		if !errors.As(err, &me) || me.Code != models.CodeContainerAddressInUse || me.Details["held_by_container_name"] != holder {
			t.Fatalf("MarkStarting(%s) = %v, want container_address_in_use held by %s", id, err, holder)
		}
		got, err := s.Get(ctx, id)
		if err != nil || got.StartedOnce || got.State != models.ContainerStopped || got.DesiredState != models.ContainerStopped {
			t.Fatalf("after the refusal: %+v, %v (the update must not have been written)", got, err)
		}
	}
	refused(b.ID, "a") // a is starting
	jid := 3
	if err := s.SaveObserved(ctx, a.ID, models.ContainerRunning, &jid, "", at); err != nil {
		t.Fatal(err)
	}
	refused(b.ID, "a") // a is running
	// Another bridge, a DHCP jail, another address: not held.
	for _, id := range []string{other.ID, dhcp.ID, c.ID} {
		if err := s.MarkStarting(ctx, id, at); err != nil {
			t.Fatalf("MarkStarting(%s) = %v", id, err)
		}
	}
	// a stopped (or failed: never running again without a start): b starts.
	if err := s.SaveObserved(ctx, a.ID, models.ContainerStopped, nil, "", at); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkStarting(ctx, b.ID, at); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkStarting(ctx, "f1a1a1a1-1111-4222-8333-444455556666", at); !errors.Is(err, models.ErrNotFound) {
		t.Fatalf("MarkStarting(unknown) = %v", err)
	}
}
