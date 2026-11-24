package localapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/store"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Prefix is the path prefix of version 1 of the local API. Like the
// agent's internal API it is not the public API and promises nothing to
// third parties.
const Prefix = "/cluster/v1"

// Paths of the operations.
const (
	PathHealth            = Prefix + "/health"
	PathStatus            = Prefix + "/status"
	PathRaftConfiguration = Prefix + "/raft/configuration"
)

// CodeForbidden is the error code for a caller an operation does not
// admit.
const CodeForbidden = "forbidden"

// Operation is one operation of the local API and who may call it.
type Operation struct {
	Name   string
	Method string
	Path   string
	// Root and API say whether root (UID 0) and the API account (skep)
	// may call the operation.
	Root bool
	API  bool
}

// Operations is the whole local API of step 8a. Every operation is a
// read. The documentation's table is checked against this list.
var Operations = []Operation{
	{Name: "health", Method: http.MethodGet, Path: PathHealth, Root: true, API: true},
	{Name: "status", Method: http.MethodGet, Path: PathStatus, Root: true, API: true},
	// The Raft configuration is what "skep-cluster listen" (root)
	// checks before it changes anything. skep-api has no use for it.
	{Name: "raft-configuration", Method: http.MethodGet, Path: PathRaftConfiguration, Root: true, API: false},
}

// Status is the answer of the status operation. Everything in it is
// public within the cluster: IDs, names, addresses and key pins.
type Status struct {
	NodeID    string `json:"node_id"`
	NodeName  string `json:"node_name"`
	ClusterID string `json:"cluster_id"`
	// Pin is the pin of this node's key.
	Pin string `json:"pin"`
	// CertificateNotAfter is when the node certificate expires.
	CertificateNotAfter time.Time `json:"certificate_not_after"`
	// ListenAddress is the configured node-to-node address ("ip:port"),
	// or "" on a single host. Listening says whether it is open.
	ListenAddress string `json:"listen_address"`
	Listening     bool   `json:"listening"`
	// Raft is the library's view; its applied index counts entries a
	// stalled state machine did not apply.
	Raft store.Stats `json:"raft"`
	// StateMachine is the replicated state machine's own applied index,
	// the one catch-up and promotion use (ADR 0013 amendment 1).
	StateMachine StateMachineStatus    `json:"state_machine"`
	Members      []clusterstate.Member `json:"members"`
	Build        string                `json:"build"`
}

// StateMachineStatus is the state machine's part of Status.
type StateMachineStatus struct {
	AppliedIndex uint64 `json:"applied_index"`
	// Stalled is set when the machine stopped at an entry this build
	// cannot apply.
	Stalled *clusterstate.Stall `json:"stalled,omitempty"`
}

// RaftConfiguration is the answer of the raft-configuration operation.
type RaftConfiguration struct {
	Servers []store.Server `json:"servers"`
}

// Backend is what the handlers need from the service.
type Backend interface {
	Health(ctx context.Context) models.Health
	Status(ctx context.Context) Status
	RaftConfiguration(ctx context.Context) (RaftConfiguration, error)
}

// HandlerOptions configures the handler.
type HandlerOptions struct {
	// APIUID is the UID of the account skep-api runs as.
	APIUID uint32
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// NewHandler returns the local API. It must be served on a Listener with
// ConnContext: a request without a verified peer is refused.
func NewHandler(b Backend, o HandlerOptions) http.Handler {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	serve := map[string]http.HandlerFunc{
		PathHealth: func(w http.ResponseWriter, r *http.Request) {
			h := b.Health(r.Context())
			code := http.StatusOK
			if h.Status == models.HealthUnavailable {
				code = http.StatusServiceUnavailable
			}
			httpkit.WriteJSON(w, r, code, h)
		},
		PathStatus: func(w http.ResponseWriter, r *http.Request) {
			httpkit.WriteJSON(w, r, http.StatusOK, b.Status(r.Context()))
		},
		PathRaftConfiguration: func(w http.ResponseWriter, r *http.Request) {
			c, err := b.RaftConfiguration(r.Context())
			if err != nil {
				httpkit.WriteError(w, r, http.StatusServiceUnavailable, "cluster_store_unavailable", err.Error())
				return
			}
			httpkit.WriteJSON(w, r, http.StatusOK, c)
		},
	}
	mux := http.NewServeMux()
	for _, op := range Operations {
		mux.Handle(op.Path, httpkit.Methods(authorize(op, o.APIUID, log, serve[op.Path]), op.Method))
	}
	mux.HandleFunc("/", httpkit.NotFound)
	var root http.Handler = mux
	root = withTimeout(root)
	root = httpkit.WithRecovery(log, root)
	root = httpkit.WithRequestID(root)
	return root
}

// authorize admits the operation's callers and refuses everyone else,
// before the operation runs. A request without a verified peer is refused
// whatever the operation.
func authorize(op Operation, apiUID uint32, log *slog.Logger, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		peer, ok := PeerFrom(r.Context())
		if !ok {
			httpkit.WriteError(w, r, http.StatusForbidden, CodeForbidden, "the peer of this connection is not verified")
			return
		}
		// root is checked first: if the API account were ever UID 0
		// it would be root, not the API.
		allowed := (peer.UID == 0 && op.Root) || (peer.UID != 0 && peer.UID == apiUID && op.API)
		if !allowed {
			log.Warn("cluster socket: operation refused for this caller",
				"operation", op.Name, "peer_uid", peer.UID, "request_id", httpkit.RequestID(r.Context()))
			httpkit.WriteError(w, r, http.StatusForbidden, CodeForbidden,
				"the operation "+op.Name+" is not open to this caller")
			return
		}
		next(w, r)
	}
}

func withTimeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
