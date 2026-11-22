package vm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// TestCreateRefusesJailHeldMAC: a VM create whose NIC has a MAC address a
// Skep jail holds is refused with 409 vm_mac_in_use naming the jail (ADR
// 0010 amendment u, 25.5 point 3), audited as denied, before any task, row,
// or dataset is made. The address is compared normalized, so an upper-case
// request matches the stored lower-case address.
func TestCreateRefusesJailHeldMAC(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var nodeID string
	if err := f.db.QueryRow(`SELECT id FROM nodes LIMIT 1`).Scan(&nodeID); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)
	tpl := models.Template{ID: "3f2a9c1e-7b4d-4e8f-9a01-23456789abcd", Name: "minimal-151", Pool: "devpool",
		Source: models.TemplateSourcePkgbase, Packages: []string{"FreeBSD-set-minimal"}, CreatedAt: at}
	jail := models.Container{ID: "a1a1a1a1-1111-4222-8333-444455556666", Name: "web1", Hostname: "web1", TemplateID: tpl.ID, CreatedAt: at,
		NICs: []models.ContainerNIC{{ID: "a2a2a2a2-1111-4222-8333-444455556666", Slot: 0, MACAddress: "02:1e:08:e9:9d:7c",
			Bridge: "bridge0", IPv4: models.ContainerIPv4{Mode: models.IPv4DHCP}}}}
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.NewTemplateStore(f.db).InsertTx(ctx, tx, nodeID, tpl); err != nil {
		t.Fatal(err)
	}
	if err := database.NewContainerStore(f.db).InsertTx(ctx, tx, nodeID, jail); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	rowsBefore := f.count(`SELECT COUNT(*) FROM virtual_machines`)
	tasksBefore := f.count(`SELECT COUNT(*) FROM tasks`)
	datasetsBefore := len(f.fake.Datasets())
	auditBefore := f.count(`SELECT COUNT(*) FROM audit_log`)

	req := basicVM("vm-jail-mac")
	req.NICs[0].MACAddress = "02:1E:08:E9:9D:7C"
	_, err = f.svc.CreateVM(ctx, req, f.sub)
	var me *models.Error
	if !errors.As(err, &me) || me.Code != models.CodeVMMACInUse {
		t.Fatalf("err = %v, want vm_mac_in_use", err)
	}
	if me.Details["held_by_kind"] != "container" || me.Details["held_by_name"] != "web1" || me.Details["held_by_id"] != jail.ID ||
		me.Details["mac_address"] != "02:1e:08:e9:9d:7c" || me.Details["slot"] != 0 {
		t.Errorf("details = %v", me.Details)
	}
	rec := httptest.NewRecorder()
	httpkit.Fail(slog.New(slog.NewTextHandler(io.Discard, nil)), rec, httptest.NewRequest("POST", "/api/v1/vms", nil), err)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), `"code":"vm_mac_in_use"`) || !strings.Contains(rec.Body.String(), `"held_by_name":"web1"`) {
		t.Errorf("API reply %d %s", rec.Code, rec.Body)
	}
	if n := f.count(`SELECT COUNT(*) FROM audit_log WHERE id > ? AND action = 'vm.create' AND phase = 'requested'
		AND outcome = 'denied' AND task_id IS NULL AND details LIKE '%"error_code":"vm_mac_in_use"%'`, auditBefore); n != 1 {
		t.Errorf("the refusal is not audited as denied (%d entries)", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM virtual_machines`); n != rowsBefore {
		t.Errorf("%d VM rows, want %d", n, rowsBefore)
	}
	if n := f.count(`SELECT COUNT(*) FROM tasks`); n != tasksBefore {
		t.Errorf("%d tasks, want %d", n, tasksBefore)
	}
	if n := len(f.fake.Datasets()); n != datasetsBefore {
		t.Errorf("%d datasets, want %d", n, datasetsBefore)
	}

	// Another address goes through.
	req.NICs[0].MACAddress = "02:1e:08:e9:9d:7d"
	f.create(req)
}

// TestCreateRefusesVMHeldMAC: a VM create whose NIC has a MAC address
// another Skep VM holds is refused with 409 vm_mac_in_use naming that VM,
// audited as denied, before any task or row is made. A replay of the
// first create with the same idempotency key still returns the first
// answer: the check runs after the replay lookup.
func TestCreateRefusesVMHeldMAC(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.sub.IdempotencyKey = "create-vm-a"
	first := basicVM("vm-a")
	first.NICs[0].MACAddress = "02:1e:08:e9:9d:7c"
	created, err := f.svc.CreateVM(ctx, first, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	if task := f.wait(created.Task.ID); task.Status != models.TaskSucceeded {
		t.Fatalf("vm.create: %s %s", task.Status, task.ErrorMessage)
	}
	replay, err := f.svc.CreateVM(ctx, first, f.sub)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.Task.ID != created.Task.ID || replay.VM.ID != created.VM.ID {
		t.Errorf("replay = task %s vm %s, first = task %s vm %s", replay.Task.ID, replay.VM.ID, created.Task.ID, created.VM.ID)
	}
	f.sub.IdempotencyKey = ""

	rowsBefore := f.count(`SELECT COUNT(*) FROM virtual_machines`)
	tasksBefore := f.count(`SELECT COUNT(*) FROM tasks`)
	auditBefore := f.count(`SELECT COUNT(*) FROM audit_log`)

	req := basicVM("vm-b")
	req.NICs[0].MACAddress = "02:1E:08:E9:9D:7C"
	_, err = f.svc.CreateVM(ctx, req, f.sub)
	var me *models.Error
	if !errors.As(err, &me) || me.Code != models.CodeVMMACInUse {
		t.Fatalf("err = %v, want vm_mac_in_use", err)
	}
	if me.Details["held_by_kind"] != "vm" || me.Details["held_by_name"] != "vm-a" || me.Details["held_by_id"] != created.VM.ID ||
		me.Details["mac_address"] != "02:1e:08:e9:9d:7c" || me.Details["slot"] != 0 {
		t.Errorf("details = %v", me.Details)
	}
	rec := httptest.NewRecorder()
	httpkit.Fail(slog.New(slog.NewTextHandler(io.Discard, nil)), rec, httptest.NewRequest("POST", "/api/v1/vms", nil), err)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), `"code":"vm_mac_in_use"`) || !strings.Contains(rec.Body.String(), `"held_by_kind":"vm"`) {
		t.Errorf("API reply %d %s", rec.Code, rec.Body)
	}
	if n := f.count(`SELECT COUNT(*) FROM audit_log WHERE id > ? AND action = 'vm.create' AND phase = 'requested'
		AND outcome = 'denied' AND task_id IS NULL AND details LIKE '%"error_code":"vm_mac_in_use"%'`, auditBefore); n != 1 {
		t.Errorf("the refusal is not audited as denied (%d entries)", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM virtual_machines`); n != rowsBefore {
		t.Errorf("%d VM rows, want %d", n, rowsBefore)
	}
	if n := f.count(`SELECT COUNT(*) FROM tasks`); n != tasksBefore {
		t.Errorf("%d tasks, want %d", n, tasksBefore)
	}
}
