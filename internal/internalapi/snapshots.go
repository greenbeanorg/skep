package internalapi

import (
	"net/http"

	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// pathIDs reads and checks the IDs of a snapshot route.
func pathIDs(w http.ResponseWriter, r *http.Request, withSnapshot bool) (vmID, snapID string, ok bool) {
	vmID = r.PathValue("id")
	if !idRE.MatchString(vmID) {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "invalid VM ID")
		return "", "", false
	}
	if withSnapshot {
		snapID = r.PathValue("sid")
		if !idRE.MatchString(snapID) {
			httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "invalid snapshot ID")
			return "", "", false
		}
	}
	return vmID, snapID, true
}

func (h *handler) snapshotCollection(w http.ResponseWriter, r *http.Request) {
	vmID, _, ok := pathIDs(w, r, false)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		var req models.SnapshotCreate
		if !decode(w, r, &req) {
			return
		}
		created, err := h.vms.CreateSnapshot(r.Context(), vmID, req, submission(r))
		if err != nil {
			httpkit.Fail(h.log, w, r, err)
			return
		}
		httpkit.WriteJSON(w, r, http.StatusAccepted, created)
		return
	}
	list, err := h.vms.Snapshots(r.Context(), vmID)
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, list)
}

func (h *handler) snapshotItem(w http.ResponseWriter, r *http.Request) {
	vmID, snapID, ok := pathIDs(w, r, true)
	if !ok {
		return
	}
	if r.Method == http.MethodDelete {
		task, err := h.vms.DeleteSnapshot(r.Context(), vmID, snapID, submission(r))
		if err != nil {
			httpkit.Fail(h.log, w, r, err)
			return
		}
		httpkit.WriteJSON(w, r, http.StatusAccepted, task)
		return
	}
	snap, err := h.vms.Snapshot(r.Context(), vmID, snapID)
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, snap)
}

func (h *handler) snapshotRollback(w http.ResponseWriter, r *http.Request) {
	vmID, snapID, ok := pathIDs(w, r, true)
	if !ok {
		return
	}
	var req models.SnapshotRollback
	if !decode(w, r, &req) {
		return
	}
	task, err := h.vms.RollbackSnapshot(r.Context(), vmID, snapID, req, submission(r))
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusAccepted, task)
}

func (h *handler) vmClone(w http.ResponseWriter, r *http.Request) {
	vmID, _, ok := pathIDs(w, r, false)
	if !ok {
		return
	}
	var req models.VMClone
	if !decode(w, r, &req) {
		return
	}
	created, err := h.vms.CloneVM(r.Context(), vmID, req, submission(r))
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusAccepted, created)
}
