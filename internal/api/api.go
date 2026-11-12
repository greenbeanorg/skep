// Package api is the HTTP layer of the Skep REST API under /api/v1.
//
// Handlers translate between HTTP and the Backend interface and contain no
// business logic. In 0.1.0 the agent serves this API itself; because the
// package depends only on Backend, it can be mounted in skep-api later
// without changes.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"

	"github.com/greenbeanorg/skep/pkg/models"
)

// Prefix is the path prefix of API version 1.
const Prefix = "/api/v1"

// Backend is what the handlers need from the service layer.
type Backend interface {
	Health(ctx context.Context) models.Health
	System(ctx context.Context) (models.System, error)
	Hardware(ctx context.Context, nodeID string) (models.Hardware, error)
	Tasks(ctx context.Context, opts models.TaskListOptions) (models.TaskList, error)
	Task(ctx context.Context, id string) (models.TaskDetail, error)
}

// nodeIDRE is the accepted shape of a resource ID in a URL.
var nodeIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type handler struct {
	backend Backend
	log     *slog.Logger
}

// NewHandler returns the API as an http.Handler, wrapped in the request ID,
// access log, panic recovery, and security header middleware.
func NewHandler(backend Backend, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	h := &handler{backend: backend, log: log}

	mux := http.NewServeMux()
	mux.Handle(Prefix+"/health", readOnly(h.health))
	mux.Handle(Prefix+"/system", readOnly(h.system))
	mux.Handle(Prefix+"/nodes/{id}/hardware", readOnly(h.nodeHardware))
	mux.Handle(Prefix+"/tasks", readOnly(h.tasks))
	mux.Handle(Prefix+"/tasks/{id}", readOnly(h.task))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, CodeNotFound, "no such endpoint: "+r.URL.Path)
	})

	var root http.Handler = mux
	root = withRecovery(log, root)
	root = withAccessLog(log, root)
	root = withSecurityHeaders(root)
	root = withRequestID(root)
	return root
}

// readOnly allows GET and HEAD and answers anything else with a JSON 405.
// Routing by path and checking the method here, instead of using method
// patterns in the mux, keeps 405 responses in the standard error envelope.
func readOnly(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
				"method "+r.Method+" is not allowed on this endpoint")
			return
		}
		next(w, r)
	})
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	health := h.backend.Health(r.Context())
	status := http.StatusOK
	if health.Status != models.HealthOK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, r, status, health)
}

func (h *handler) system(w http.ResponseWriter, r *http.Request) {
	sys, err := h.backend.System(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, sys)
}

func (h *handler) nodeHardware(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !nodeIDRE.MatchString(id) {
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "invalid node ID")
		return
	}
	hw, err := h.backend.Hardware(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, hw)
}

// tasks lists tasks, newest first. Tasks are read-only over the API in this
// release: nothing can be submitted or cancelled over the network until the
// API has authentication, because every task needs an actor.
func (h *handler) tasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := models.TaskListOptions{
		Status: models.TaskStatus(q.Get("status")),
		Cursor: q.Get("cursor"),
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "limit must be a positive integer")
			return
		}
		opts.Limit = n
	}
	list, err := h.backend.Tasks(r.Context(), opts)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, list)
}

func (h *handler) task(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !nodeIDRE.MatchString(id) {
		writeError(w, r, http.StatusBadRequest, CodeInvalidRequest, "invalid task ID")
		return
	}
	detail, err := h.backend.Task(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, detail)
}

// statusForCode maps the stable error codes of models.Error to HTTP
// statuses. A code that is not listed is treated as an internal error.
var statusForCode = map[string]int{
	models.CodeInvalidRequest:      http.StatusBadRequest,
	models.CodeResourceBusy:        http.StatusConflict,
	models.CodeIdempotencyConflict: http.StatusConflict,
	models.CodeNotCancellable:      http.StatusConflict,
	models.CodeQueueFull:           http.StatusServiceUnavailable,
	models.CodeShuttingDown:        http.StatusServiceUnavailable,
}

// fail maps a service error to an error response. Internal error text is
// logged and never sent to the client.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var me *models.Error
	if errors.As(err, &me) {
		if status, ok := statusForCode[me.Code]; ok {
			writeErrorDetails(w, r, status, me.Code, me.Message, me.Details)
			return
		}
	}
	switch {
	case errors.Is(err, models.ErrNotFound):
		writeError(w, r, http.StatusNotFound, CodeNotFound, "the requested resource does not exist")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "the request was cancelled or timed out")
	default:
		h.log.ErrorContext(r.Context(), "api: request failed",
			"error", err, "path", r.URL.Path, "request_id", requestID(r.Context()))
		writeError(w, r, http.StatusInternalServerError, CodeInternal, "internal server error")
	}
}
