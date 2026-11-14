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
	StoragePools(ctx context.Context) (models.StoragePoolList, error)
	Media(ctx context.Context) (models.MediaList, error)
}

// registerVMs adds the VM and storage routes. Like every route of the
// internal API they carry typed values only: no command, no path.
func (h *handler) registerVMs(mux *http.ServeMux) {
	mux.Handle(agentproto.Prefix+"/vms", httpkit.Methods(h.vmCollection, http.MethodGet, http.MethodHead, http.MethodPost))
	mux.Handle(agentproto.Prefix+"/vms/{id}", httpkit.Methods(h.vmItem,
		http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete))
	mux.Handle(agentproto.Prefix+"/storage/pools", httpkit.ReadOnly(h.storagePools))
	mux.Handle(agentproto.Prefix+"/storage/media", httpkit.ReadOnly(h.storageMedia))
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
