package internalapi

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Operation is one operation of the agent's internal API and who may call
// it (step 8d-1, D2), as localapi.Operations is for skep-cluster's socket.
type Operation struct {
	Name   string
	Method string
	// Path is below agentproto.Prefix.
	Path string
	// Root and API say whether root (UID 0) and the API account may call
	// the operation.
	Root bool
	API  bool
	// Part is the backend the operation needs ("" for the core); without
	// it the route is not mounted.
	Part string
}

// Parts of the table: the backends of Options.
const (
	partVMs        = "vms"
	partConsole    = "console"
	partStorage    = "storage"
	partTemplates  = "templates"
	partContainers = "containers"
	partMetrics    = "metrics"
	partEngine     = "engine"
)

// Operations is the whole internal API. Every route is registered from
// it; a route not in it does not exist. The documentation's table is
// checked against it. Every operation before step 8d-1 is open to both
// callers; the drain's set and clear are root's alone (D3).
var Operations = []Operation{
	{"health", http.MethodGet, "/health", true, true, ""},
	{"system", http.MethodGet, "/system", true, true, ""},
	{"node-hardware", http.MethodGet, "/nodes/{id}/hardware", true, true, ""},
	{"tasks-list", http.MethodGet, "/tasks", true, true, ""},
	{"task-get", http.MethodGet, "/tasks/{id}", true, true, ""},
	{"task-cancel", http.MethodPost, "/tasks/{id}/cancel", true, true, ""},
	{"task-by-key", http.MethodGet, "/tasks/by-key/{key}", true, true, partEngine},
	{"drain-get", http.MethodGet, "/drain", true, true, partEngine},
	{"drain-set", http.MethodPost, "/drain", true, false, partEngine},
	{"drain-clear", http.MethodDelete, "/drain", true, false, partEngine},

	{"vms-list", http.MethodGet, "/vms", true, true, partVMs},
	{"vm-create", http.MethodPost, "/vms", true, true, partVMs},
	{"vm-get", http.MethodGet, "/vms/{id}", true, true, partVMs},
	{"vm-update", http.MethodPut, "/vms/{id}", true, true, partVMs},
	{"vm-delete", http.MethodDelete, "/vms/{id}", true, true, partVMs},
	{"vm-action", http.MethodPost, "/vms/{id}/actions", true, true, partVMs},
	{"vm-console", http.MethodGet, "/vms/{id}/console/serial", true, true, partConsole},
	{"vm-snapshots-list", http.MethodGet, "/vms/{id}/snapshots", true, true, partVMs},
	{"vm-snapshot-create", http.MethodPost, "/vms/{id}/snapshots", true, true, partVMs},
	{"vm-snapshot-get", http.MethodGet, "/vms/{id}/snapshots/{sid}", true, true, partVMs},
	{"vm-snapshot-delete", http.MethodDelete, "/vms/{id}/snapshots/{sid}", true, true, partVMs},
	{"vm-snapshot-rollback", http.MethodPost, "/vms/{id}/snapshots/{sid}/rollback", true, true, partVMs},
	{"vm-clone", http.MethodPost, "/vms/{id}/clone", true, true, partVMs},
	{"storage-pools", http.MethodGet, "/storage/pools", true, true, partVMs},
	{"storage-media", http.MethodGet, "/storage/media", true, true, partVMs},
	{"storage-datasets", http.MethodGet, "/storage/datasets", true, true, partStorage},

	{"template-sources", http.MethodGet, "/storage/template-sources", true, true, partTemplates},
	{"templates-list", http.MethodGet, "/storage/templates", true, true, partTemplates},
	{"template-create", http.MethodPost, "/storage/templates", true, true, partTemplates},
	{"template-get", http.MethodGet, "/storage/templates/{id}", true, true, partTemplates},
	{"template-delete", http.MethodDelete, "/storage/templates/{id}", true, true, partTemplates},

	{"containers-list", http.MethodGet, "/containers", true, true, partContainers},
	{"container-create", http.MethodPost, "/containers", true, true, partContainers},
	{"container-get", http.MethodGet, "/containers/{id}", true, true, partContainers},
	{"container-update", http.MethodPut, "/containers/{id}", true, true, partContainers},
	{"container-delete", http.MethodDelete, "/containers/{id}", true, true, partContainers},
	{"container-action", http.MethodPost, "/containers/{id}/actions", true, true, partContainers},
	{"container-console-log", http.MethodGet, "/containers/{id}/console-log", true, true, partContainers},
	{"container-snapshots-list", http.MethodGet, "/containers/{id}/snapshots", true, true, partContainers},
	{"container-snapshot-create", http.MethodPost, "/containers/{id}/snapshots", true, true, partContainers},
	{"container-snapshot-get", http.MethodGet, "/containers/{id}/snapshots/{sid}", true, true, partContainers},
	{"container-snapshot-delete", http.MethodDelete, "/containers/{id}/snapshots/{sid}", true, true, partContainers},
	{"container-snapshot-rollback", http.MethodPost, "/containers/{id}/snapshots/{sid}/rollback", true, true, partContainers},
	{"container-clone", http.MethodPost, "/containers/{id}/clone", true, true, partContainers},

	{"metrics", http.MethodGet, "/metrics", true, true, partMetrics},
}

// EngineBackend is the task engine's drain and its lookup by idempotency
// key (*tasks.Engine).
type EngineBackend interface {
	SetDrain(ctx context.Context, reason string, by tasks.DrainActor) (tasks.Drain, int, error)
	ClearDrain(ctx context.Context, by tasks.DrainActor)
	CurrentDrain(ctx context.Context) (tasks.Drain, bool, int)
	LookupKey(ctx context.Context, key string) (tasks.Task, bool, error)
}

// DrainStatus is the answer of the drain operations: the active drain, or
// none (empty reason), and the unfinished tasks.
type DrainStatus struct {
	Reason          string     `json:"reason,omitempty"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	UnfinishedTasks int        `json:"unfinished_tasks"`
}

// DrainRequest is the body of drain-set.
type DrainRequest struct {
	Reason string `json:"reason"`
}

// mount registers every operation of the table whose part is present,
// each behind authorize, by method and path. A path that exists with
// another method is a JSON 405, anything else a JSON 404.
func (h *handler) mount(byPath map[string]http.HandlerFunc) http.Handler {
	mux := http.NewServeMux()
	var mounted []Operation
	for _, op := range Operations {
		fn, ok := byPath[op.Path]
		if !ok {
			continue
		}
		mux.Handle(op.Method+" "+agentproto.Prefix+op.Path, h.authorize(op, fn))
		mounted = append(mounted, op)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		var allow []string
		for _, op := range mounted {
			if pathMatches(agentproto.Prefix+op.Path, r.URL.Path) {
				allow = append(allow, op.Method)
				if op.Method == http.MethodGet {
					allow = append(allow, http.MethodHead)
				}
			}
		}
		if len(allow) == 0 {
			httpkit.NotFound(w, r)
			return
		}
		w.Header().Set("Allow", strings.Join(allow, ", "))
		httpkit.WriteError(w, r, http.StatusMethodNotAllowed, httpkit.CodeMethodNotAllowed,
			"method "+r.Method+" is not allowed on this endpoint")
	})
}

// pathMatches reports whether path matches pattern, where a "{name}"
// segment matches one non-empty segment.
func pathMatches(pattern, path string) bool {
	ps, xs := strings.Split(pattern, "/"), strings.Split(path, "/")
	if len(ps) != len(xs) {
		return false
	}
	for i, p := range ps {
		if strings.HasPrefix(p, "{") {
			if xs[i] == "" {
				return false
			}
		} else if p != xs[i] {
			return false
		}
	}
	return true
}

// authorize admits the operation's callers by the kernel's peer UID and
// refuses everyone else, before the operation runs: root first, then the
// API account (an API account that were UID 0 would be root, not the
// API). No header takes part in the decision.
func (h *handler) authorize(op Operation, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		peer, ok := PeerFrom(r.Context())
		if !ok {
			httpkit.WriteError(w, r, http.StatusForbidden, CodeForbidden, "the peer of this connection is not verified")
			return
		}
		if !h.mayCall(op, peer.UID) {
			h.log.Warn("agent socket: operation refused for this caller",
				"operation", op.Name, "peer_uid", peer.UID, "request_id", httpkit.RequestID(r.Context()))
			httpkit.WriteError(w, r, http.StatusForbidden, CodeForbidden, "the operation "+op.Name+" is not open to this caller")
			return
		}
		next(w, r)
	}
}

func (h *handler) mayCall(op Operation, uid uint32) bool {
	if uid == 0 {
		return op.Root
	}
	// Without a configured list every other peer the listener admitted
	// is the API account (Options.APIUIDs).
	return op.API && (h.apiUIDs == nil || slices.Contains(h.apiUIDs, uid))
}

// keyRE is the idempotency key's shape (the engine's).
var keyRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func (h *handler) taskByKey(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !keyRE.MatchString(key) {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "invalid idempotency key")
		return
	}
	t, found, err := h.engine.LookupKey(r.Context(), key)
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	if !found {
		httpkit.WriteError(w, r, http.StatusNotFound, httpkit.CodeNotFound, "no task was created with this idempotency key")
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, t.Public())
}

// drainActor is who sets or clears a drain: the headers' actor and the
// kernel's peer UID (both checked before any handler ran).
func drainActor(r *http.Request) tasks.DrainActor {
	caller, _ := agentproto.CallerFrom(r.Context())
	peer, _ := PeerFrom(r.Context())
	return tasks.DrainActor{Actor: models.TaskActor{ID: caller.ActorID, Name: caller.ActorName}, PeerUID: peer.UID, RequestID: caller.RequestID}
}

func drainStatus(d tasks.Drain, active bool, unfinished int) DrainStatus {
	s := DrainStatus{UnfinishedTasks: unfinished}
	if active {
		at := d.ExpiresAt.UTC()
		s.Reason, s.ExpiresAt = d.Reason, &at
	}
	return s
}

// drain serves the three drain operations; authorize has admitted the
// caller for the method.
func (h *handler) drain(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req DrainRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || dec.More() {
			httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, `the body must be {"reason":"upgrade"|"join"}`)
			return
		}
		d, n, err := h.engine.SetDrain(r.Context(), req.Reason, drainActor(r))
		if err != nil {
			httpkit.Fail(h.log, w, r, err)
			return
		}
		httpkit.WriteJSON(w, r, http.StatusOK, drainStatus(d, true, n))
	case http.MethodDelete:
		h.engine.ClearDrain(r.Context(), drainActor(r))
		d, active, n := h.engine.CurrentDrain(r.Context())
		httpkit.WriteJSON(w, r, http.StatusOK, drainStatus(d, active, n))
	default:
		d, active, n := h.engine.CurrentDrain(r.Context())
		httpkit.WriteJSON(w, r, http.StatusOK, drainStatus(d, active, n))
	}
}
