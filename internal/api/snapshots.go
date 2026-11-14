package api

import (
	"net/http"

	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Snapshots and clones of VMs (ADR 0010 decisions 2, 3, and 14). Every
// write is audited in the access log, with the VM as the resource; the
// agent's log has the operation, under the same request ID.

func snapshotIDs(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	id, ok := vmID(w, r)
	if !ok {
		return "", "", false
	}
	snapID := r.PathValue("sid")
	if !nodeIDRE.MatchString(snapID) {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "invalid snapshot ID")
		return "", "", false
	}
	return id, snapID, true
}

func (h *handler) listSnapshots(w http.ResponseWriter, r *http.Request) {
	id, ok := vmID(w, r)
	if !ok {
		return
	}
	list, err := h.vms.Snapshots(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, list)
}

func (h *handler) getSnapshot(w http.ResponseWriter, r *http.Request) {
	id, snapID, ok := snapshotIDs(w, r)
	if !ok {
		return
	}
	snap, err := h.vms.Snapshot(r.Context(), id, snapID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, snap)
}

func (h *handler) createSnapshot(w http.ResponseWriter, r *http.Request) {
	id, ok := vmID(w, r)
	if !ok {
		return
	}
	var req models.SnapshotCreate
	if !decodeBody(w, r, &req) {
		return
	}
	if r, ok = withIdempotencyKey(w, r); !ok {
		return
	}
	created, err := h.vms.CreateSnapshot(r.Context(), id, req)
	h.auditVM(r, models.TaskVMSnapshotCreate, id, &created.Task, err)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", Prefix+"/vms/"+id+"/snapshots/"+created.Snapshot.ID)
	httpkit.WriteJSON(w, r, http.StatusAccepted, created)
}

func (h *handler) rollbackSnapshot(w http.ResponseWriter, r *http.Request) {
	id, snapID, ok := snapshotIDs(w, r)
	if !ok {
		return
	}
	var req models.SnapshotRollback
	if !decodeBody(w, r, &req) {
		return
	}
	if r, ok = withIdempotencyKey(w, r); !ok {
		return
	}
	task, err := h.vms.RollbackSnapshot(r.Context(), id, snapID, req)
	h.auditVM(r, models.TaskVMSnapshotRollback, id, &task, err)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusAccepted, task)
}

func (h *handler) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id, snapID, ok := snapshotIDs(w, r)
	if !ok {
		return
	}
	if r, ok = withIdempotencyKey(w, r); !ok {
		return
	}
	task, err := h.vms.DeleteSnapshot(r.Context(), id, snapID)
	h.auditVM(r, models.TaskVMSnapshotDelete, id, &task, err)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusAccepted, task)
}

// cloneVM asks for a linked clone. The answer is 202 with the clone, not
// yet provisioned, as for a create.
func (h *handler) cloneVM(w http.ResponseWriter, r *http.Request) {
	id, ok := vmID(w, r)
	if !ok {
		return
	}
	var req models.VMClone
	if !decodeBody(w, r, &req) {
		return
	}
	if r, ok = withIdempotencyKey(w, r); !ok {
		return
	}
	created, err := h.vms.CloneVM(r.Context(), id, req)
	h.auditVM(r, models.TaskVMClone, id, &created.Task, err)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", Prefix+"/vms/"+created.VM.ID)
	httpkit.WriteJSON(w, r, http.StatusAccepted, created)
}
