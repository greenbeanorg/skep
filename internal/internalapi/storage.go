package internalapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// StorageBackend is what the storage handlers need from the agent's
// storage service.
type StorageBackend interface {
	Datasets(ctx context.Context, opts models.DatasetListOptions) (models.DatasetList, error)
}

// datasetListOptions reads the inventory's query parameters.
func datasetListOptions(r *http.Request) (models.DatasetListOptions, bool) {
	q := r.URL.Query()
	opts := models.DatasetListOptions{Pool: q.Get("pool"), Cursor: q.Get("cursor")}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return opts, false
		}
		opts.Limit = n
	}
	return opts, true
}

func (h *handler) storageDatasets(w http.ResponseWriter, r *http.Request) {
	opts, ok := datasetListOptions(r)
	if !ok {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "limit must be a positive integer")
		return
	}
	list, err := h.storage.Datasets(r.Context(), opts)
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, list)
}
