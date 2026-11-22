package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/migrations"
	"github.com/greenbeanorg/skep/pkg/models"
)

func TestContainerStore(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db, migrations.Agent()); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO nodes (id, name, is_local) VALUES ('n1', 'node1', 1)`)
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	tpls := NewTemplateStore(db)
	tpl := models.Template{ID: "3f2a9c1e-7b4d-4e8f-9a01-23456789abcd", Name: "minimal-151", Pool: "skeptest",
		Source: models.TemplateSourcePkgbase, Packages: []string{"FreeBSD-set-minimal"}, CreatedAt: at}
	tx, _ := db.BeginTx(ctx, nil)
	if err := tpls.InsertTx(ctx, tx, "n1", tpl); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	if err := tpls.SetProvisioned(ctx, tpl.ID, "15.1-RELEASE-p6", "", 1, at); err != nil {
		t.Fatal(err)
	}

	s := NewContainerStore(db)
	insert := func(c models.Container) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := s.InsertTx(ctx, tx, "n1", c); err != nil {
			return err
		}
		return tx.Commit()
	}
	a := models.Container{ID: "a1a1a1a1-1111-4222-8333-444455556666", Name: "web1", Hostname: "web1.lab",
		TemplateID: tpl.ID, Options: models.ContainerOptions{SysVIPC: true}, SSHAuthorizedKey: "ssh-ed25519 AAAA k", CreatedAt: at}
	b := models.Container{ID: "b1b1b1b1-1111-4222-8333-444455556666", Name: "db1", Hostname: "db1", TemplateID: tpl.ID, CreatedAt: at}
	for _, c := range []models.Container{a, b} {
		if err := insert(c); err != nil {
			t.Fatal(err)
		}
	}
	dup := b
	dup.ID = "c1c1c1c1-1111-4222-8333-444455556666"
	var me *models.Error
	if err := insert(dup); !errors.As(err, &me) || me.Code != models.CodeContainerNameTaken {
		t.Fatalf("a repeated name: %v", err)
	}
	orphan := dup
	orphan.Name, orphan.TemplateID = "x", "9f2a9c1e-7b4d-4e8f-9a01-23456789abcd"
	if err := insert(orphan); !errors.Is(err, models.ErrNotFound) {
		t.Fatalf("an unknown template: %v", err)
	}

	got, err := s.Get(ctx, a.ID)
	if err != nil || got.Name != "web1" || got.Hostname != "web1.lab" || got.TemplateName != "minimal-151" ||
		got.Pool != "skeptest" || got.Release != "15.1-RELEASE-p6" || !got.Options.SysVIPC || got.Options.Mlock ||
		got.SSHAuthorizedKey != "ssh-ed25519 AAAA k" || got.Provisioned || got.StartedOnce ||
		got.DesiredState != models.ContainerStopped || got.State != models.ContainerUnknown || got.JID != nil {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := s.Get(ctx, "nope"); !errors.Is(err, models.ErrNotFound) {
		t.Fatalf("Get missing = %v", err)
	}

	page, next, err := s.List(ctx, 1, "")
	if err != nil || len(page) != 1 || page[0].Name != "db1" || next == "" {
		t.Fatalf("List page 1 = %+v, %q, %v", page, next, err)
	}
	page, next, err = s.List(ctx, 1, next)
	if err != nil || len(page) != 1 || page[0].Name != "web1" || next != "" {
		t.Fatalf("List page 2 = %+v, %q, %v", page, next, err)
	}
	if ids, err := s.IDs(ctx); err != nil || len(ids) != 2 {
		t.Fatalf("IDs = %v, %v", ids, err)
	}

	later := at.Add(time.Minute)
	if err := s.SetProvisioned(ctx, a.ID, later); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkStarting(ctx, a.ID, later); err != nil {
		t.Fatal(err)
	}
	jid := 12
	if err := s.SaveObserved(ctx, a.ID, models.ContainerRunning, &jid, "", later); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, a.ID)
	if !got.Provisioned || !got.StartedOnce || got.DesiredState != models.ContainerRunning ||
		got.State != models.ContainerRunning || got.JID == nil || *got.JID != 12 || got.ObservedAt == nil {
		t.Fatalf("after start: %+v", got)
	}
	// started_once never goes back (the schema's trigger).
	if _, err := db.ExecContext(ctx, `UPDATE containers SET started_once = 0 WHERE id = ?`, a.ID); err == nil {
		t.Fatal("started_once went back to 0")
	}
	if err := s.SaveObserved(ctx, a.ID, models.ContainerStopped, nil, "removed outside Skep", later); err != nil {
		t.Fatal(err)
	}
	got.Hostname, got.SSHAuthorizedKey, got.Options = "web2", "", models.ContainerOptions{}
	if err := s.UpdateConfig(ctx, got, later); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, a.ID)
	if got.JID != nil || got.StateDetail != "removed outside Skep" || got.Hostname != "web2" || got.SSHAuthorizedKey != "" || got.Options.SysVIPC {
		t.Fatalf("after update: %+v", got)
	}
	// A template in use cannot be deleted.
	if err := tpls.Delete(ctx, tpl.ID); err == nil {
		t.Fatal("a template in use was deleted")
	}
	for _, id := range []string{a.ID, b.ID} {
		if err := s.Delete(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Delete(ctx, a.ID); !errors.Is(err, models.ErrNotFound) {
		t.Fatalf("a second delete: %v", err)
	}
	if err := tpls.Delete(ctx, tpl.ID); err != nil {
		t.Fatalf("an unused template: %v", err)
	}
}

// TestContainerStoreMACHeld: the store's own check, in the writing
// transaction, refuses a NIC whose MAC a VM or another jail holds with
// container_mac_in_use and held_by_kind (amendment u, finding 2), on insert
// and on update; the jail's own address is not a conflict.
func TestContainerStoreMACHeld(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db, migrations.Agent()); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO nodes (id, name, is_local) VALUES ('n1', 'node1', 1)`)
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	tpls := NewTemplateStore(db)
	tpl := models.Template{ID: "3f2a9c1e-7b4d-4e8f-9a01-23456789abcd", Name: "minimal-151", Pool: "skeptest",
		Source: models.TemplateSourcePkgbase, Packages: []string{"FreeBSD-set-minimal"}, CreatedAt: at}
	tx, _ := db.BeginTx(ctx, nil)
	if err := tpls.InsertTx(ctx, tx, "n1", tpl); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	mustExec(t, db, `INSERT INTO virtual_machines (id, node_id, name, engine, cpu_count, memory_bytes)
		VALUES ('aaaaaaaa-0000-4000-8000-000000000001', 'n1', 'vm1', 'bhyve', 1, 268435456)`)
	mustExec(t, db, `INSERT INTO vm_networks (id, vm_id, slot, mac_address, bridge)
		VALUES ('aaaaaaaa-0000-4000-8000-000000000002', 'aaaaaaaa-0000-4000-8000-000000000001', 0, '02:1e:08:e9:9d:7c', 'bridge0')`)
	s := NewContainerStore(db)
	insert := func(c models.Container) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := s.InsertTx(ctx, tx, "n1", c); err != nil {
			return err
		}
		return tx.Commit()
	}
	nic := func(id, mac string) []models.ContainerNIC {
		return []models.ContainerNIC{{ID: id, Slot: 0, MACAddress: mac, Bridge: "bridge0", IPv4: models.ContainerIPv4{Mode: models.IPv4DHCP}}}
	}
	held := func(err error, kind string) {
		t.Helper()
		var me *models.Error
		if !errors.As(err, &me) || me.Code != models.CodeContainerMACInUse || me.Details["held_by_kind"] != kind {
			t.Fatalf("err = %v (%+v), want container_mac_in_use held by a %s", err, me, kind)
		}
	}
	a := models.Container{ID: "a1a1a1a1-1111-4222-8333-444455556666", Name: "web1", Hostname: "web1", TemplateID: tpl.ID, CreatedAt: at,
		NICs: nic("a2a2a2a2-1111-4222-8333-444455556666", "02:1e:08:e9:9d:7c")}
	held(insert(a), "vm")
	a.NICs = nic("a2a2a2a2-1111-4222-8333-444455556666", "02:00:00:00:00:0a")
	if err := insert(a); err != nil {
		t.Fatal(err)
	}
	b := models.Container{ID: "b1b1b1b1-1111-4222-8333-444455556666", Name: "db1", Hostname: "db1", TemplateID: tpl.ID, CreatedAt: at,
		NICs: nic("b2b2b2b2-1111-4222-8333-444455556666", "02:00:00:00:00:0a")}
	held(insert(b), "container")
	a.NICs = nic("a2a2a2a2-1111-4222-8333-444455556666", "02:1e:08:e9:9d:7c")
	held(s.UpdateConfig(ctx, a, at), "vm")
	a.NICs = nic("a2a2a2a2-1111-4222-8333-444455556666", "02:00:00:00:00:0a")
	if err := s.UpdateConfig(ctx, a, at); err != nil {
		t.Fatalf("the jail's own MAC: %v", err)
	}
}

// TestVMStoreMACHeldByJail: the VM side (ADR 0010 amendment u, 25.5 point
// 3). A VM NIC whose MAC a jail holds is refused with vm_mac_in_use and the
// jail named in the details, both by CheckMACsFree and by InsertTx in the
// writing transaction; a MAC another VM holds is vm_mac_in_use too, naming
// the VM; a free MAC is stored.
func TestVMStoreMACHeldByJail(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	if _, err := Migrate(ctx, db, migrations.Agent()); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO nodes (id, name, is_local) VALUES ('n1', 'node1', 1)`)
	at := time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)
	tpls := NewTemplateStore(db)
	tpl := models.Template{ID: "3f2a9c1e-7b4d-4e8f-9a01-23456789abcd", Name: "minimal-151", Pool: "skeptest",
		Source: models.TemplateSourcePkgbase, Packages: []string{"FreeBSD-set-minimal"}, CreatedAt: at}
	tx, _ := db.BeginTx(ctx, nil)
	if err := tpls.InsertTx(ctx, tx, "n1", tpl); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	cs := NewContainerStore(db)
	jail := models.Container{ID: "a1a1a1a1-1111-4222-8333-444455556666", Name: "web1", Hostname: "web1", TemplateID: tpl.ID, CreatedAt: at,
		NICs: []models.ContainerNIC{{ID: "a2a2a2a2-1111-4222-8333-444455556666", Slot: 1, MACAddress: "02:00:00:00:00:0a",
			Bridge: "bridge0", IPv4: models.ContainerIPv4{Mode: models.IPv4DHCP}}}}
	tx, _ = db.BeginTx(ctx, nil)
	if err := cs.InsertTx(ctx, tx, "n1", jail); err != nil {
		t.Fatal(err)
	}
	tx.Commit()

	s := NewVMStore(db)
	vm := func(id, name, nicID, mac string) models.VM {
		return models.VM{ID: id, NodeID: "n1", Name: name, Engine: "bhyve", CPUCount: 1, MemoryBytes: 256 << 20,
			Firmware: "uefi", CreatedAt: at, UpdatedAt: at,
			NICs: []models.VMNIC{{ID: nicID, Slot: 0, Model: "virtio-net", MACAddress: mac, Bridge: "bridge0"}}}
	}
	insert := func(v models.VM) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := s.InsertTx(ctx, tx, v, "skep"); err != nil {
			return err
		}
		return tx.Commit()
	}
	held := func(err error) {
		t.Helper()
		var me *models.Error
		if !errors.As(err, &me) || me.Code != models.CodeVMMACInUse {
			t.Fatalf("err = %v, want vm_mac_in_use", err)
		}
		want := map[string]any{"slot": 0, "mac_address": "02:00:00:00:00:0a", "held_by_kind": "container",
			"held_by_id": jail.ID, "held_by_name": "web1"}
		for k, v := range want {
			if me.Details[k] != v {
				t.Errorf("details[%s] = %v, want %v", k, me.Details[k], v)
			}
		}
		if !strings.Contains(me.Message, "container web1 (slot 1)") {
			t.Errorf("message %q does not name the jail and its slot", me.Message)
		}
	}

	v1 := vm("bbbbbbbb-0000-4000-8000-000000000001", "vm1", "bbbbbbbb-0000-4000-8000-000000000002", "02:00:00:00:00:0a")
	held(s.CheckMACsFree(ctx, v1.NICs))
	held(insert(v1))
	if _, err := s.Get(ctx, v1.ID); !errors.Is(err, models.ErrNotFound) {
		t.Fatalf("a refused VM was stored: %v", err)
	}

	v1.NICs[0].MACAddress = "02:00:00:00:00:0b"
	if err := s.CheckMACsFree(ctx, v1.NICs); err != nil {
		t.Fatalf("a free MAC: %v", err)
	}
	if err := insert(v1); err != nil {
		t.Fatalf("a free MAC: %v", err)
	}
	// Another VM's MAC: vm_mac_in_use too, naming the VM, from both checks.
	v2 := vm("cccccccc-0000-4000-8000-000000000001", "vm2", "cccccccc-0000-4000-8000-000000000002", "02:00:00:00:00:0b")
	heldByVM := func(err error) {
		t.Helper()
		var me *models.Error
		if !errors.As(err, &me) || me.Code != models.CodeVMMACInUse {
			t.Fatalf("err = %v, want vm_mac_in_use", err)
		}
		want := map[string]any{"slot": 0, "mac_address": "02:00:00:00:00:0b", "held_by_kind": "vm",
			"held_by_id": v1.ID, "held_by_name": "vm1"}
		for k, v := range want {
			if me.Details[k] != v {
				t.Errorf("details[%s] = %v, want %v", k, me.Details[k], v)
			}
		}
		if !strings.Contains(me.Message, "VM vm1 (slot 0)") {
			t.Errorf("message %q does not name the VM and its slot", me.Message)
		}
	}
	heldByVM(s.CheckMACsFree(ctx, v2.NICs))
	heldByVM(insert(v2))
	if _, err := s.Get(ctx, v2.ID); !errors.Is(err, models.ErrNotFound) {
		t.Fatalf("a refused VM was stored: %v", err)
	}
	// A NIC whose address is still to be generated is not checked.
	if err := s.CheckMACsFree(ctx, []models.VMNIC{{Slot: 0}}); err != nil {
		t.Fatalf("an empty MAC: %v", err)
	}
}
