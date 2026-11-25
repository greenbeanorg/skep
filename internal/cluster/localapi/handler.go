package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	PathAdmit             = Prefix + "/admit"
	PathWithdraw          = Prefix + "/admit/withdraw"
	PathRemove            = Prefix + "/remove"
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

// Operations is the whole local API. The documentation's table is checked
// against this list.
var Operations = []Operation{
	{Name: "health", Method: http.MethodGet, Path: PathHealth, Root: true, API: true},
	{Name: "status", Method: http.MethodGet, Path: PathStatus, Root: true, API: true},
	// The Raft configuration is what "skep-cluster listen" (root)
	// checks before it changes anything. skep-api has no use for it.
	{Name: "raft-configuration", Method: http.MethodGet, Path: PathRaftConfiguration, Root: true, API: false},
	// Admitting a host and withdrawing an admission are root-only in
	// step 8b; skep-api gets them, with cluster:manage, in step 8c
	// (ADR 0013 B2).
	{Name: "admit", Method: http.MethodPost, Path: PathAdmit, Root: true, API: false},
	{Name: "admit-withdraw", Method: http.MethodPost, Path: PathWithdraw, Root: true, API: false},
	// Removing a member is root-only in step 8b-2a, like admit; skep-api
	// gets it with cluster:manage in step 8c (decision D3).
	{Name: "remove", Method: http.MethodPost, Path: PathRemove, Root: true, API: false},
}

// AdmitRequest is the body of the admit operation.
type AdmitRequest struct {
	// Request is the line "skep-cluster join-request" printed.
	Request string `json:"request"`
}

// AdmitResult is the answer of the admit operation.
type AdmitResult struct {
	// Admission is the line to paste into "skep-cluster join".
	Admission string `json:"admission"`
	ClusterID string `json:"cluster_id"`
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	Pin       string `json:"pin"`
	ExpiresAt int64  `json:"expires_at"`
	OpID      string `json:"op_id"`
}

// WithdrawRequest is the body of the admit-withdraw operation.
type WithdrawRequest struct {
	Name string `json:"name"`
}

// WithdrawResult is its answer.
type WithdrawResult struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
	OpID   string `json:"op_id"`
}

// RemoveRequest is the body of the remove operation.
type RemoveRequest struct {
	Name string `json:"name"`
	// AllowUnanswered removes the member although too few of the
	// remaining voters answered a status round (decision D7c). It needs
	// ClusterID, typed back by the administrator.
	AllowUnanswered bool   `json:"allow_unanswered,omitempty"`
	ClusterID       string `json:"cluster_id,omitempty"`
}

// RemoveResult is its answer: the first entry (removing) is committed.
type RemoveResult struct {
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	OpID      string `json:"op_id"`
	ClusterID string `json:"cluster_id"`
	// HandedOver is true when the node was the leader and handed the
	// leadership over first (decision D6).
	HandedOver bool `json:"handed_over,omitempty"`
	// Unanswered names the remaining voters that did not answer the
	// status round of the removal (only with AllowUnanswered).
	Unanswered []string `json:"unanswered,omitempty"`
}

// RemovedStatus is Status for a node that was removed.
type RemovedStatus struct {
	ClusterID string `json:"cluster_id"`
	At        string `json:"at"`
	How       string `json:"how"`
}

// OpError is an error answer of an operation that changes something.
type OpError struct {
	HTTPStatus int
	Code       string
	Message    string
}

func (e *OpError) Error() string { return e.Code + ": " + e.Message }

// JoinStatus is a joining node's part of Status.
type JoinStatus struct {
	Phase     string `json:"phase"`
	ClusterID string `json:"cluster_id"`
	Message   string `json:"message,omitempty"`
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
	// Pendings are the admitted hosts that have not joined.
	Pendings []clusterstate.Pending `json:"pendings"`
	// Servers is the latest Raft configuration.
	Servers []store.Server `json:"servers,omitempty"`
	// AcceptedBy says where the keys the listener accepts come from:
	// "state machine", "membership file" (after a restart, until the
	// machine has caught up), "admission" (joining) or "none".
	AcceptedBy string `json:"accepted_by"`
	// Join is set while this node is joining.
	Join *JoinStatus `json:"join,omitempty"`
	// Removed is set on a node that was removed from its cluster.
	Removed *RemovedStatus `json:"removed,omitempty"`
	// Removals says, for each member being removed, what its removal
	// waits for.
	Removals []string `json:"removals,omitempty"`
	// UnappliedRemoval names the members that have not yet applied the
	// last removal, from the last status round (decision D12).
	UnappliedRemoval []string `json:"unapplied_removal,omitempty"`
	// Clocks is, per other member, its clock offset from this node's as
	// the last status rounds measured it (D3 of step 8b-2b).
	Clocks []ClockStatus `json:"clocks,omitempty"`
	Build  string        `json:"build"`
}

// ClockStatus is one member's clock as this node measured it.
type ClockStatus struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
	// Known is false while there is no sample: none yet, a build
	// without the clock operation, or no answer (Note says which).
	Known bool `json:"known"`
	// OffsetMillis is the member's time minus this node's, at the
	// midpoint of the request; UncertaintyMillis is half the round trip.
	OffsetMillis      int64     `json:"offset_ms,omitempty"`
	UncertaintyMillis int64     `json:"uncertainty_ms,omitempty"`
	RoundTripMillis   int64     `json:"round_trip_ms,omitempty"`
	At                time.Time `json:"at,omitempty"`
	Note              string    `json:"note,omitempty"`
}

// StateMachineStatus is the state machine's part of Status.
type StateMachineStatus struct {
	AppliedIndex uint64 `json:"applied_index"`
	// Stalled is set when the machine stopped at an entry this build
	// cannot apply.
	Stalled *clusterstate.Stall `json:"stalled,omitempty"`
	// UpToDate is true when the machine is not stopped, the library has
	// applied the commit index, and every entry between the machine's
	// index and the library's is one of the library's own (no-op,
	// configuration change): "applied 7 of 10" is then no lag.
	UpToDate bool `json:"up_to_date"`
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
	// Admit and Withdraw return an *OpError for a refusal.
	Admit(ctx context.Context, request, actor string) (AdmitResult, error)
	Withdraw(ctx context.Context, name, actor string) (WithdrawResult, error)
	Remove(ctx context.Context, req RemoveRequest, actor string) (RemoveResult, error)
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
		PathAdmit: func(w http.ResponseWriter, r *http.Request) {
			var req AdmitRequest
			if !decodeBody(w, r, &req) {
				return
			}
			res, err := b.Admit(r.Context(), req.Request, actorOf(r))
			writeResult(w, r, res, err)
		},
		PathRemove: func(w http.ResponseWriter, r *http.Request) {
			var req RemoveRequest
			if !decodeBody(w, r, &req) {
				return
			}
			res, err := b.Remove(r.Context(), req, actorOf(r))
			writeResult(w, r, res, err)
		},
		PathWithdraw: func(w http.ResponseWriter, r *http.Request) {
			var req WithdrawRequest
			if !decodeBody(w, r, &req) {
				return
			}
			res, err := b.Withdraw(r.Context(), req.Name, actorOf(r))
			writeResult(w, r, res, err)
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

// Time limits of one request. The remove operation has its own: it may
// make a status round, hand the leadership over, wait for a new leader
// and pass the removal on to it.
const (
	// RequestTimeout is the limit of every operation but remove.
	RequestTimeout = 15 * time.Second
	// RemoveTimeout is the limit of the remove operation.
	RemoveTimeout = 60 * time.Second
	// WriteTimeout is the HTTP server's write limit; it must be above
	// every operation's limit.
	WriteTimeout = RemoveTimeout + 15*time.Second
)

func withTimeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		limit := RequestTimeout
		if r.URL.Path == PathRemove {
			limit = RemoveTimeout
		}
		ctx, cancel := context.WithTimeout(r.Context(), limit)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// actorOf names the caller of an operation that changes something: the
// kernel-verified peer UID on this node.
func actorOf(r *http.Request) string {
	if peer, ok := PeerFrom(r.Context()); ok && peer.UID == 0 {
		return "root"
	}
	return "unknown"
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r.Body); err != nil {
		httpkit.WriteError(w, r, http.StatusBadRequest, "invalid_request", "cannot read the request body")
		return false
	}
	dec := json.NewDecoder(&buf)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.More() {
		httpkit.WriteError(w, r, http.StatusBadRequest, "invalid_request", "the request body is not valid JSON for this operation")
		return false
	}
	return true
}

func writeResult(w http.ResponseWriter, r *http.Request, res any, err error) {
	if err == nil {
		httpkit.WriteJSON(w, r, http.StatusOK, res)
		return
	}
	var oe *OpError
	if errors.As(err, &oe) {
		httpkit.WriteError(w, r, oe.HTTPStatus, oe.Code, oe.Message)
		return
	}
	httpkit.WriteError(w, r, http.StatusInternalServerError, "internal_error", err.Error())
}
