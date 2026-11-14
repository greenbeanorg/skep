package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/pkg/models"
)

// TestVMLifecycleEndToEnd: a VM is created, read, updated, and deleted
// through the public API, across the socket, by the agent's VM service on
// the simulated host. Both audit logs record each write with the same
// request ID, and the agent's entry names the verified peer.
func TestVMLifecycleEndToEnd(t *testing.T) {
	s := newStack(t)

	create := []byte(`{"name":"web1","cpu_count":2,"memory":"1G",
		"disks":[{"size":"8G","pool":"devpool","thin":true},{"media":"install.iso"}],
		"nics":[{"bridge":"bridge0"}]}`)

	// A viewer may read but not create.
	if rec := s.call(http.MethodPost, "/api/v1/vms", auth.RoleViewer, create, "req-vm-viewer"); rec.Code != http.StatusForbidden {
		t.Fatalf("create as viewer: %d %s", rec.Code, rec.Body)
	}
	if rec := s.call(http.MethodGet, "/api/v1/vms", auth.RoleViewer, nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("list as viewer: %d %s", rec.Code, rec.Body)
	}

	// An operator creates it.
	rec := s.call(http.MethodPost, "/api/v1/vms", auth.RoleOperator, create, "req-vm-create")
	var created models.VMCreated
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || rec.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	id := created.VM.ID
	if id == "" || created.Task.Type != models.TaskVMCreate || created.Task.ResourceID != id {
		t.Fatalf("created = %+v", created)
	}
	s.waitStatus(t, created.Task.ID, models.TaskSucceeded)

	rec = s.call(http.MethodGet, "/api/v1/vms/"+id, auth.RoleViewer, nil, "")
	var vm models.VM
	if err := json.Unmarshal(rec.Body.Bytes(), &vm); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("read: %d %s", rec.Code, rec.Body)
	}
	if !vm.Provisioned || vm.Name != "web1" || len(vm.Disks) != 2 || len(vm.NICs) != 1 {
		t.Fatalf("vm = %+v", vm)
	}
	wantDS := "devpool/skep/vm/" + id + "/disk0"
	found := false
	for _, ds := range s.fake.Datasets() {
		found = found || ds == wantDS
	}
	if !found {
		t.Errorf("datasets = %v, want %s", s.fake.Datasets(), wantDS)
	}

	// The agent's record names the user, the verified peer, and the
	// request ID the API used.
	if n := s.count(t, s.agentDB, `SELECT COUNT(*) FROM audit_log WHERE action = 'vm.create' AND actor_id = ?
		AND peer_uid = ? AND request_id = 'req-vm-create'`, s.userIDs[auth.RoleOperator], os.Geteuid()); n < 2 {
		t.Errorf("%d vm.create entries in the agent's audit log, want requested and finished", n)
	}
	if n := s.count(t, s.apiDB, `SELECT COUNT(*) FROM audit_log WHERE action = 'vm.create' AND outcome = 'success'
		AND resource_id = ? AND request_id = 'req-vm-create'`, id); n != 1 {
		t.Errorf("%d vm.create entries in the access audit log, want 1", n)
	}

	// A pool outside the allow-list is refused and audited on both sides;
	// nothing is created.
	before := len(s.fake.Datasets())
	bad := []byte(`{"name":"web2","cpu_count":1,"memory":"512M","disks":[{"size":"1G","pool":"tank"}]}`)
	rec = s.call(http.MethodPost, "/api/v1/vms", auth.RoleOperator, bad, "req-vm-pool")
	var apiErr models.ErrorResponse
	json.Unmarshal(rec.Body.Bytes(), &apiErr)
	if rec.Code != http.StatusUnprocessableEntity || apiErr.Error.Code != models.CodePoolNotAllowed {
		t.Fatalf("create on a pool not allowed: %d %s", rec.Code, rec.Body)
	}
	if len(s.fake.Datasets()) != before {
		t.Error("a refused create made a dataset")
	}
	if n := s.count(t, s.agentDB, `SELECT COUNT(*) FROM audit_log WHERE action = 'vm.create' AND outcome = 'denied'
		AND request_id = 'req-vm-pool'`); n != 1 {
		t.Errorf("%d denied vm.create entries in the agent's audit log, want 1", n)
	}
	if n := s.count(t, s.apiDB, `SELECT COUNT(*) FROM audit_log WHERE action = 'vm.create' AND outcome = 'denied'
		AND request_id = 'req-vm-pool'`); n != 1 {
		t.Errorf("%d denied vm.create entries in the access audit log, want 1", n)
	}

	// Detach the media.
	rec = s.call(http.MethodPut, "/api/v1/vms/"+id, auth.RoleOperator, []byte(`{"media":[]}`), "req-vm-update")
	var task models.Task
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil || rec.Code != http.StatusAccepted {
		t.Fatalf("update: %d %s", rec.Code, rec.Body)
	}
	s.waitStatus(t, task.ID, models.TaskSucceeded)
	rec = s.call(http.MethodGet, "/api/v1/vms/"+id, auth.RoleViewer, nil, "")
	vm = models.VM{}
	json.Unmarshal(rec.Body.Bytes(), &vm)
	if len(vm.Disks) != 1 {
		t.Errorf("disks after detaching the media = %+v", vm.Disks)
	}

	// Storage listings.
	for _, path := range []string{"/api/v1/storage/pools", "/api/v1/storage/media"} {
		if rec := s.call(http.MethodGet, path, auth.RoleViewer, nil, ""); rec.Code != http.StatusOK {
			t.Errorf("%s as viewer: %d %s", path, rec.Code, rec.Body)
		}
	}

	// A viewer cannot delete; an operator can.
	if rec := s.call(http.MethodDelete, "/api/v1/vms/"+id, auth.RoleViewer, nil, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("delete as viewer: %d", rec.Code)
	}
	rec = s.call(http.MethodDelete, "/api/v1/vms/"+id, auth.RoleOperator, nil, "req-vm-delete")
	task = models.Task{}
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil || rec.Code != http.StatusAccepted {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	s.waitStatus(t, task.ID, models.TaskSucceeded)
	if rec := s.call(http.MethodGet, "/api/v1/vms/"+id, auth.RoleViewer, nil, ""); rec.Code != http.StatusNotFound {
		t.Errorf("read after delete: %d", rec.Code)
	}
	for _, ds := range s.fake.Datasets() {
		if ds == wantDS {
			t.Errorf("the zvol %s survived the delete", ds)
		}
	}
	if n := s.count(t, s.agentDB, `SELECT COUNT(*) FROM audit_log WHERE action = 'vm.delete' AND phase = 'finished'
		AND details LIKE '%datasets_destroyed%'`); n != 1 {
		t.Errorf("%d vm.delete finished entries listing what was destroyed, want 1", n)
	}
}
