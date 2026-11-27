package internalapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/internal/vm"
	"github.com/greenbeanorg/skep/pkg/models"
)

// VMBackend is what the VM and storage handlers need from the agent's VM
// service. Every write names who it is for: the actor skep-api asserted
// and the peer the kernel verified.
type VMBackend interface {
	VMs(ctx context.Context, opts models.VMListOptions) (models.VMList, error)
	VM(ctx context.Context, id string) (models.VM, error)
	CreateVM(ctx context.Context, req models.VMCreate, sub vm.Submission) (models.VMCreated, error)
	UpdateVM(ctx context.Context, id string, upd models.VMUpdate, sub vm.Submission) (models.Task, error)
	DeleteVM(ctx context.Context, id string, sub vm.Submission) (models.Task, error)
	Action(ctx context.Context, id string, act models.VMAction, sub vm.Submission) (models.Task, error)
	StoragePools(ctx context.Context) (models.StoragePoolList, error)
	Media(ctx context.Context) (models.MediaList, error)

	Snapshots(ctx context.Context, vmID string) (models.SnapshotList, error)
	Snapshot(ctx context.Context, vmID, id string) (models.Snapshot, error)
	CreateSnapshot(ctx context.Context, vmID string, req models.SnapshotCreate, sub vm.Submission) (models.SnapshotCreated, error)
	RollbackSnapshot(ctx context.Context, vmID, id string, req models.SnapshotRollback, sub vm.Submission) (models.Task, error)
	DeleteSnapshot(ctx context.Context, vmID, id string, sub vm.Submission) (models.Task, error)
	CloneVM(ctx context.Context, vmID string, req models.VMClone, sub vm.Submission) (models.VMCreated, error)
}

// registerVMs adds the VM and storage routes. Like every route of the
// internal API they carry typed values only: no command, no path.
func (h *handler) registerVMs(add func(string, http.HandlerFunc)) {
	add("/vms", h.vmCollection)
	add("/vms/{id}", h.vmItem)
	add("/vms/{id}/actions", h.vmAction)
	if h.console != nil {
		add("/vms/{id}/console/serial", h.vmConsole)
	}
	add("/vms/{id}/snapshots", h.snapshotCollection)
	add("/vms/{id}/snapshots/{sid}", h.snapshotItem)
	add("/vms/{id}/snapshots/{sid}/rollback", h.snapshotRollback)
	add("/vms/{id}/clone", h.vmClone)
	add("/storage/pools", h.storagePools)
	add("/storage/media", h.storageMedia)
}

// submission builds who a write is for from the request: the actor from
// the headers skep-api sent, the peer UID from the kernel.
func submission(r *http.Request) vm.Submission {
	// Both were checked by the middleware before any handler ran.
	caller, _ := agentproto.CallerFrom(r.Context())
	peer, _ := PeerFrom(r.Context())
	return vm.Submission{
		Actor:          models.TaskActor{ID: caller.ActorID, Name: caller.ActorName},
		PeerUID:        peer.UID,
		RequestID:      caller.RequestID,
		SourceIP:       caller.SourceIP,
		IdempotencyKey: caller.IdempotencyKey,
		OriginNode:     caller.OriginNode,
		Envelope:       caller.Envelope,
	}
}

// decode reads the JSON body of a request into v. The agent validates
// again whatever skep-api validated: this is the security control.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "the request body is not the expected JSON object")
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "unexpected data after the JSON object")
		return false
	}
	return true
}

func (h *handler) vmCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req models.VMCreate
		if !decode(w, r, &req) {
			return
		}
		created, err := h.vms.CreateVM(r.Context(), req, submission(r))
		if err != nil {
			httpkit.Fail(h.log, w, r, err)
			return
		}
		httpkit.WriteJSON(w, r, http.StatusAccepted, created)
		return
	}
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
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, list)
}

func (h *handler) vmItem(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRE.MatchString(id) {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "invalid VM ID")
		return
	}
	switch r.Method {
	case http.MethodPut:
		var upd models.VMUpdate
		if !decode(w, r, &upd) {
			return
		}
		task, err := h.vms.UpdateVM(r.Context(), id, upd, submission(r))
		if err != nil {
			httpkit.Fail(h.log, w, r, err)
			return
		}
		httpkit.WriteJSON(w, r, http.StatusAccepted, task)
	case http.MethodDelete:
		task, err := h.vms.DeleteVM(r.Context(), id, submission(r))
		if err != nil {
			httpkit.Fail(h.log, w, r, err)
			return
		}
		httpkit.WriteJSON(w, r, http.StatusAccepted, task)
	default:
		found, err := h.vms.VM(r.Context(), id)
		if err != nil {
			httpkit.Fail(h.log, w, r, err)
			return
		}
		httpkit.WriteJSON(w, r, http.StatusOK, found)
	}
}

func (h *handler) vmAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRE.MatchString(id) {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "invalid VM ID")
		return
	}
	var act models.VMAction
	if !decode(w, r, &act) {
		return
	}
	task, err := h.vms.Action(r.Context(), id, act, submission(r))
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusAccepted, task)
}

func (h *handler) storagePools(w http.ResponseWriter, r *http.Request) {
	list, err := h.vms.StoragePools(r.Context())
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, list)
}

func (h *handler) storageMedia(w http.ResponseWriter, r *http.Request) {
	list, err := h.vms.Media(r.Context())
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, list)
}
