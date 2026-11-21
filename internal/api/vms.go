package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// VMBackend is what the VM and storage handlers need from the service
// layer: in skep-api, the agent client. Every write is made for the caller
// in ctx and becomes a task on the agent.
type VMBackend interface {
	VMs(ctx context.Context, opts models.VMListOptions) (models.VMList, error)
	VM(ctx context.Context, id string) (models.VM, error)
	CreateVM(ctx context.Context, req models.VMCreate) (models.VMCreated, error)
	UpdateVM(ctx context.Context, id string, upd models.VMUpdate) (models.Task, error)
	DeleteVM(ctx context.Context, id string) (models.Task, error)
	VMAction(ctx context.Context, id string, act models.VMAction) (models.Task, error)
	StoragePools(ctx context.Context) (models.StoragePoolList, error)
	StorageDatasets(ctx context.Context, opts models.DatasetListOptions) (models.DatasetList, error)
	Media(ctx context.Context) (models.MediaList, error)

	Snapshots(ctx context.Context, vmID string) (models.SnapshotList, error)
	Snapshot(ctx context.Context, vmID, id string) (models.Snapshot, error)
	CreateSnapshot(ctx context.Context, vmID string, req models.SnapshotCreate) (models.SnapshotCreated, error)
	RollbackSnapshot(ctx context.Context, vmID, id string, req models.SnapshotRollback) (models.Task, error)
	DeleteSnapshot(ctx context.Context, vmID, id string) (models.Task, error)
	CloneVM(ctx context.Context, vmID string, req models.VMClone) (models.VMCreated, error)

	Metrics(ctx context.Context) (models.Metrics, error)
}

// vmRoutes is the VM and storage part of the route table (ADR 0009
// decisions 9 and 10).
func (h *handler) vmRoutes() []route {
	return []route{
		{http.MethodGet, "/vms", auth.PermVMRead, nil, h.listVMs},
		{http.MethodPost, "/vms", auth.PermVMWrite, nil, h.createVM},
		{http.MethodGet, "/vms/{id}", auth.PermVMRead, nil, h.getVM},
		{http.MethodPut, "/vms/{id}", auth.PermVMWrite, nil, h.updateVM},
		{http.MethodDelete, "/vms/{id}", auth.PermVMDelete, nil, h.deleteVM},
		{http.MethodPost, "/vms/{id}/actions", auth.PermVMPower, nil, h.vmAction},
		{http.MethodGet, "/vms/{id}/console/serial", auth.PermVMConsole, nil, h.vmConsole},
		{http.MethodGet, "/vms/{id}/snapshots", auth.PermVMRead, nil, h.listSnapshots},
		{http.MethodPost, "/vms/{id}/snapshots", auth.PermVMSnapshot, nil, h.createSnapshot},
		{http.MethodGet, "/vms/{id}/snapshots/{sid}", auth.PermVMRead, nil, h.getSnapshot},
		{http.MethodDelete, "/vms/{id}/snapshots/{sid}", auth.PermVMSnapshot, nil, h.deleteSnapshot},
		{http.MethodPost, "/vms/{id}/snapshots/{sid}/rollback", auth.PermVMSnapshot, nil, h.rollbackSnapshot},
		{http.MethodPost, "/vms/{id}/clone", auth.PermVMWrite, []string{auth.PermVMSnapshot}, h.cloneVM},
		{http.MethodGet, "/storage/pools", auth.PermStorageRead, nil, h.listPools},
		{http.MethodGet, "/storage/datasets", auth.PermStorageRead, nil, h.listDatasets},
		{http.MethodGet, "/storage/media", auth.PermStorageRead, nil, h.listMedia},
		// The metrics name VMs, jails, and pools, so they need what
		// reading each needs (ADR 0010 decision 15).
		{http.MethodGet, "/metrics", auth.PermSystemRead, []string{auth.PermVMRead, auth.PermStorageRead, auth.PermContainerRead}, h.metrics},
	}
}

// noVMs answers every VM operation with "not found". It stands in when the
// handler was built without a VM backend.
type noVMs struct{}

func (noVMs) VMs(context.Context, models.VMListOptions) (models.VMList, error) {
	return models.VMList{Items: []models.VM{}}, nil
}
func (noVMs) VM(context.Context, string) (models.VM, error) { return models.VM{}, models.ErrNotFound }
func (noVMs) CreateVM(context.Context, models.VMCreate) (models.VMCreated, error) {
	return models.VMCreated{}, models.ErrNotFound
}
func (noVMs) UpdateVM(context.Context, string, models.VMUpdate) (models.Task, error) {
	return models.Task{}, models.ErrNotFound
}
func (noVMs) DeleteVM(context.Context, string) (models.Task, error) {
	return models.Task{}, models.ErrNotFound
}
func (noVMs) VMAction(context.Context, string, models.VMAction) (models.Task, error) {
	return models.Task{}, models.ErrNotFound
}
func (noVMs) StoragePools(context.Context) (models.StoragePoolList, error) {
	return models.StoragePoolList{Items: []models.StoragePool{}}, nil
}
func (noVMs) StorageDatasets(context.Context, models.DatasetListOptions) (models.DatasetList, error) {
	return models.DatasetList{Items: []models.Dataset{}}, nil
}
func (noVMs) Metrics(context.Context) (models.Metrics, error) {
	return models.Metrics{Pools: []models.PoolMetrics{}, VMs: []models.VMMetrics{}}, nil
}
func (noVMs) Media(context.Context) (models.MediaList, error) {
	return models.MediaList{Items: []models.Media{}}, nil
}
func (noVMs) Snapshots(context.Context, string) (models.SnapshotList, error) {
	return models.SnapshotList{}, models.ErrNotFound
}
func (noVMs) Snapshot(context.Context, string, string) (models.Snapshot, error) {
	return models.Snapshot{}, models.ErrNotFound
}
func (noVMs) CreateSnapshot(context.Context, string, models.SnapshotCreate) (models.SnapshotCreated, error) {
	return models.SnapshotCreated{}, models.ErrNotFound
}
func (noVMs) RollbackSnapshot(context.Context, string, string, models.SnapshotRollback) (models.Task, error) {
	return models.Task{}, models.ErrNotFound
}
func (noVMs) DeleteSnapshot(context.Context, string, string) (models.Task, error) {
	return models.Task{}, models.ErrNotFound
}
func (noVMs) CloneVM(context.Context, string, models.VMClone) (models.VMCreated, error) {
	return models.VMCreated{}, models.ErrNotFound
}

func vmID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !nodeIDRE.MatchString(id) {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "invalid VM ID")
		return "", false
	}
	return id, true
}

// withIdempotencyKey passes the request's Idempotency-Key header on to the
// agent with the caller. A malformed key is refused here.
func withIdempotencyKey(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	key := r.Header.Get(agentproto.HeaderIdempotencyKey)
	if key == "" {
		return r, true
	}
	caller, _ := agentproto.CallerFrom(r.Context())
	caller.IdempotencyKey = key
	if err := caller.Validate(); err != nil {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, err.Error())
		return r, false
	}
	return r.WithContext(agentproto.WithCaller(r.Context(), caller)), true
}

// auditVM records a VM write in the access audit log: who asked for which
// operation on which VM, and whether the agent accepted it. The operation
// itself is in the agent's log, with the same request ID.
func (h *handler) auditVM(r *http.Request, action, id string, task *models.Task, err error) {
	who, _ := identityFrom(r.Context())
	entry := audit.AccessEntry{
		ActorID: who.User.ID, ActorName: who.User.Username, Action: action,
		ResourceType: models.VMResource, ResourceID: id, Outcome: audit.Success,
	}
	switch {
	case err == nil:
		if task != nil {
			entry.Details = map[string]any{"task_id": task.ID}
		}
	default:
		entry.Outcome = audit.Failure
		var me *models.Error
		switch {
		case errors.As(err, &me):
			entry.Details = map[string]any{"error_code": me.Code}
			// Refused by a rule, not malformed and not broken.
			if me.Code != models.CodeInvalidRequest && me.Code != models.CodeAgentUnavailable &&
				me.Code != models.CodeQueueFull && me.Code != models.CodeShuttingDown {
				entry.Outcome = audit.Denied
			}
		case errors.Is(err, models.ErrNotFound):
			entry.Details = map[string]any{"error_code": httpkit.CodeNotFound}
		}
	}
	h.writeAudit(r, entry)
}

func (h *handler) listVMs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := models.VMListOptions{State: q.Get("state"), Cursor: q.Get("cursor")}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "limit must be a positive integer")
			return
		}
		opts.Limit = n
	}
	list, err := h.vms.VMs(r.Context(), opts)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, list)
}

func (h *handler) getVM(w http.ResponseWriter, r *http.Request) {
	id, ok := vmID(w, r)
	if !ok {
		return
	}
	vm, err := h.vms.VM(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, vm)
}

// createVM asks the agent to create a VM. The answer is 202: the VM
// exists, not yet provisioned, and the task provisions it.
func (h *handler) createVM(w http.ResponseWriter, r *http.Request) {
	var req models.VMCreate
	if !decodeBody(w, r, &req) {
		return
	}
	r, ok := withIdempotencyKey(w, r)
	if !ok {
		return
	}
	created, err := h.vms.CreateVM(r.Context(), req)
	if err != nil {
		h.auditVM(r, models.TaskVMCreate, "", nil, err)
		h.fail(w, r, err)
		return
	}
	h.auditVM(r, models.TaskVMCreate, created.VM.ID, &created.Task, nil)
	w.Header().Set("Location", Prefix+"/vms/"+created.VM.ID)
	httpkit.WriteJSON(w, r, http.StatusAccepted, created)
}

func (h *handler) updateVM(w http.ResponseWriter, r *http.Request) {
	id, ok := vmID(w, r)
	if !ok {
		return
	}
	var upd models.VMUpdate
	if !decodeBody(w, r, &upd) {
		return
	}
	// Removing a disk destroys its zvol: vm:delete as well (ADR 0010
	// decision 4).
	if upd.RemovesDisks() && !h.requireAlso(w, r, http.MethodPut, "/vms/{id}", auth.PermVMDelete) {
		return
	}
	if r, ok = withIdempotencyKey(w, r); !ok {
		return
	}
	task, err := h.vms.UpdateVM(r.Context(), id, upd)
	h.auditVM(r, models.TaskVMUpdate, id, &task, err)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusAccepted, task)
}

func (h *handler) deleteVM(w http.ResponseWriter, r *http.Request) {
	id, ok := vmID(w, r)
	if !ok {
		return
	}
	if r, ok = withIdempotencyKey(w, r); !ok {
		return
	}
	task, err := h.vms.DeleteVM(r.Context(), id)
	h.auditVM(r, models.TaskVMDelete, id, &task, err)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusAccepted, task)
}

// vmAction asks the agent for a power action. The audit action is the
// task type the action becomes, so both logs name it the same way.
func (h *handler) vmAction(w http.ResponseWriter, r *http.Request) {
	id, ok := vmID(w, r)
	if !ok {
		return
	}
	var act models.VMAction
	if !decodeBody(w, r, &act) {
		return
	}
	if r, ok = withIdempotencyKey(w, r); !ok {
		return
	}
	action := "vm." + act.Action
	switch act.Action {
	case models.VMActionStart, models.VMActionStop, models.VMActionForceStop, models.VMActionRestart:
	default:
		action = "vm.action"
	}
	task, err := h.vms.VMAction(r.Context(), id, act)
	h.auditVM(r, action, id, &task, err)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusAccepted, task)
}

func (h *handler) listPools(w http.ResponseWriter, r *http.Request) {
	list, err := h.vms.StoragePools(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, list)
}

func (h *handler) listMedia(w http.ResponseWriter, r *http.Request) {
	list, err := h.vms.Media(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, list)
}

// listDatasets is the storage inventory (ADR 0010 decision 1): what Skep
// owns under <pool>/<root>, unowned datasets flagged.
func (h *handler) listDatasets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := models.DatasetListOptions{Pool: q.Get("pool"), Cursor: q.Get("cursor")}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "limit must be a positive integer")
			return
		}
		opts.Limit = n
	}
	list, err := h.vms.StorageDatasets(r.Context(), opts)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, list)
}

// metrics is the node's current resource use (ADR 0010 decision 15).
func (h *handler) metrics(w http.ResponseWriter, r *http.Request) {
	m, err := h.vms.Metrics(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, m)
}
