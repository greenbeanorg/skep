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
	"github.com/greenbeanorg/skep/internal/cluster/guestapi"
	"github.com/greenbeanorg/skep/internal/cluster/memberapi"
	"github.com/greenbeanorg/skep/internal/cluster/store"
	"github.com/greenbeanorg/skep/internal/cluster/userapi"
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
	PathKeyRotate         = Prefix + "/key/rotate"
	PathKeyAbort          = Prefix + "/key/abort"
	PathUsersRead         = userapi.PathUsersRead
	PathUsersWrite        = userapi.PathUsersWrite
	PathGuestsRead        = guestapi.PathGuestsRead
	PathGuestsWrite       = guestapi.PathGuestsWrite
)

// CodeForbidden is the error code for a caller an operation does not
// admit.
const CodeForbidden = "forbidden"

// The member messages are package memberapi's (step 8c-2, D21), which
// skep-api imports without the rest of this package.
type (
	AdmitRequest    = memberapi.AdmitRequest
	AdmitResult     = memberapi.AdmitResult
	WithdrawRequest = memberapi.WithdrawRequest
	WithdrawResult  = memberapi.WithdrawResult
	RemoveRequest   = memberapi.RemoveRequest
	RemoveResult    = memberapi.RemoveResult
	JoinStatus      = memberapi.JoinStatus
	RemovedStatus   = memberapi.RemovedStatus
	ClockStatus     = memberapi.ClockStatus
)

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
	// Admitting a host, withdrawing an admission and removing a member:
	// root's commands, and skep-api's with cluster:manage (step 8c-2,
	// D22). For the API account the request names the user as its actor
	// and carries a request ID; allow_unanswered is root's alone (D23)
	// and a removal must carry the cluster ID (D24).
	{Name: "admit", Method: http.MethodPost, Path: PathAdmit, Root: true, API: true},
	{Name: "admit-withdraw", Method: http.MethodPost, Path: PathWithdraw, Root: true, API: true},
	{Name: "remove", Method: http.MethodPost, Path: PathRemove, Root: true, API: true},
	// Rotating this node's key and aborting it: root only, local; no
	// operation of skep-api changes a pin (step 8b-2b, D1; "--api"
	// arrives with forwarding, step 8e).
	{Name: "key-rotate", Method: http.MethodPost, Path: PathKeyRotate, Root: true, API: false},
	{Name: "key-abort", Method: http.MethodPost, Path: PathKeyAbort, Root: true, API: false},
	// The replicated users (step 8c-1, D11): skep-api's read copy and
	// its writes. Root has no use for them; "skep-api admin" runs as the
	// API account.
	{Name: "users-read", Method: http.MethodGet, Path: PathUsersRead, Root: false, API: true},
	{Name: "users-write", Method: http.MethodPost, Path: PathUsersWrite, Root: false, API: true},
	// The guest inventory (step 8d-1, D22, D23): skep-api's reads and
	// its writes. The owner of an entry is this node, set by the
	// service; the request has no owner field. Root reads it too, for
	// "skep-cluster remove --forget-guests" (step 8d-2, D19).
	{Name: "guests-read", Method: http.MethodGet, Path: PathGuestsRead, Root: true, API: true},
	{Name: "guests-write", Method: http.MethodPost, Path: PathGuestsWrite, Root: false, API: true},
}

// RotateResult is the answer of key-rotate and key-abort.
type RotateResult struct {
	OldPin string `json:"old_pin"`
	NewPin string `json:"new_pin"`
	OpID   string `json:"op_id"`
	Phase  string `json:"phase"`
	// AddIndex is the index of the member.key_add entry, once the leader
	// answered; Pending says why it is not known yet.
	AddIndex uint64 `json:"add_index,omitempty"`
	Pending  string `json:"pending,omitempty"`
}

// RotationStatus is this node's own key rotation in Status.
type RotationStatus struct {
	Phase   string `json:"phase"`
	OldPin  string `json:"old_pin"`
	NewPin  string `json:"new_pin"`
	OpID    string `json:"op_id"`
	Started string `json:"started"`
	// Using is the pin of the key this node uses now.
	Using string `json:"using"`
}

// KeySeen is the pin a member's last connection to or from this node
// used.
type KeySeen struct {
	NodeID string    `json:"node_id"`
	Name   string    `json:"name"`
	Pin    string    `json:"pin"`
	At     time.Time `json:"at"`
}

// OpError is an error answer of an operation that changes something.
type OpError struct {
	HTTPStatus int
	Code       string
	Message    string
}

func (e *OpError) Error() string { return e.Code + ": " + e.Message }

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
	// Rotation is this node's own key rotation, while one is under way.
	Rotation *RotationStatus `json:"rotation,omitempty"`
	// Rotations says, per member with a next pin, what its rotation
	// waits for.
	Rotations []string `json:"rotations,omitempty"`
	// KeysSeen is, per other member, the pin it last used with this node.
	KeysSeen []KeySeen `json:"keys_seen,omitempty"`
	Build    string    `json:"build"`
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
	Rotate(ctx context.Context, actor string) (RotateResult, error)
	AbortRotation(ctx context.Context, actor string) (RotateResult, error)
	// UsersRead waits up to wait for the users index to differ from
	// after, then answers the users.
	UsersRead(ctx context.Context, after uint64, wait time.Duration) (UsersAnswer, error)
	// UsersWrite returns an *OpError for a refusal.
	UsersWrite(ctx context.Context, req UsersWriteRequest) (UsersWriteResult, error)
	// GuestsRead answers the guest inventory.
	GuestsRead(ctx context.Context) (guestapi.GuestsAnswer, error)
	// GuestsWrite returns an *OpError for a refusal.
	GuestsWrite(ctx context.Context, req guestapi.WriteRequest) (guestapi.WriteResult, error)
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
			actor, ok := callerActor(w, r, log, "admit", req.Actor, req.RequestID)
			if !ok {
				return
			}
			res, err := b.Admit(r.Context(), req.Request, actor)
			writeResult(w, r, res, err)
		},
		PathRemove: func(w http.ResponseWriter, r *http.Request) {
			var req RemoveRequest
			if !decodeBody(w, r, &req) {
				return
			}
			actor, ok := callerActor(w, r, log, "remove", req.Actor, req.RequestID)
			if !ok {
				return
			}
			if actor != "root" {
				switch {
				case req.AllowUnanswered:
					httpkit.WriteError(w, r, http.StatusForbidden, CodeForbidden, "allow_unanswered is not open to this caller; only root's command may remove a member without the remaining voters' answers")
					return
				case req.ForgetGuests:
					httpkit.WriteError(w, r, http.StatusForbidden, CodeForbidden, "forget_guests is not open to this caller; only root's command may remove a member that owns guests")
					return
				case req.ClusterID == "":
					httpkit.WriteError(w, r, http.StatusBadRequest, "invalid_request", "a removal by the API must carry the cluster ID")
					return
				}
			}
			req.Actor, req.RequestID = "", ""
			res, err := b.Remove(r.Context(), req, actor)
			writeResult(w, r, res, err)
		},
		PathKeyRotate: func(w http.ResponseWriter, r *http.Request) {
			var req struct{}
			if !decodeBody(w, r, &req) {
				return
			}
			res, err := b.Rotate(r.Context(), actorOf(r))
			writeResult(w, r, res, err)
		},
		PathKeyAbort: func(w http.ResponseWriter, r *http.Request) {
			var req struct{}
			if !decodeBody(w, r, &req) {
				return
			}
			res, err := b.AbortRotation(r.Context(), actorOf(r))
			writeResult(w, r, res, err)
		},
		PathUsersRead: func(w http.ResponseWriter, r *http.Request) {
			after, wait, ok := usersReadQuery(r)
			if !ok {
				httpkit.WriteError(w, r, http.StatusBadRequest, "invalid_request", "after must be an index and wait a number of seconds from 0 to 5")
				return
			}
			res, err := b.UsersRead(r.Context(), after, wait)
			writeResult(w, r, res, err)
		},
		PathUsersWrite: func(w http.ResponseWriter, r *http.Request) {
			var req UsersWriteRequest
			if !decodeBody(w, r, &req) {
				return
			}
			res, err := b.UsersWrite(r.Context(), req)
			writeResult(w, r, res, err)
		},
		PathGuestsRead: func(w http.ResponseWriter, r *http.Request) {
			res, err := b.GuestsRead(r.Context())
			writeResult(w, r, res, err)
		},
		PathGuestsWrite: func(w http.ResponseWriter, r *http.Request) {
			var req guestapi.WriteRequest
			if !decodeBody(w, r, &req) {
				return
			}
			res, err := b.GuestsWrite(r.Context(), req)
			writeResult(w, r, res, err)
		},
		PathWithdraw: func(w http.ResponseWriter, r *http.Request) {
			var req WithdrawRequest
			if !decodeBody(w, r, &req) {
				return
			}
			actor, ok := callerActor(w, r, log, "admit-withdraw", req.Actor, req.RequestID)
			if !ok {
				return
			}
			res, err := b.Withdraw(r.Context(), req.Name, actor)
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
	// UsersReadTimeout is the limit of users-read: its longest wait and
	// the time to answer.
	UsersReadTimeout = MaxUsersWait + 5*time.Second
	// WriteTimeout is the HTTP server's write limit; it must be above
	// every operation's limit.
	WriteTimeout = RemoveTimeout + 15*time.Second
)

func withTimeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		limit := RequestTimeout
		switch r.URL.Path {
		case PathRemove:
			limit = RemoveTimeout
		case PathUsersRead:
			limit = UsersReadTimeout
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

// callerActor is the actor of a member operation (D22 of step 8c-2): for
// root "root", and a request that names an actor or a request ID is
// refused; for the API account the actor it names, which must be
// "user:<username>", with a request ID, both logged. Anything else is
// refused before the operation runs.
func callerActor(w http.ResponseWriter, r *http.Request, log *slog.Logger, op, actor, requestID string) (string, bool) {
	if actorOf(r) == "root" {
		if actor != "" || requestID != "" {
			httpkit.WriteError(w, r, http.StatusBadRequest, "invalid_request", "root's requests name no actor and no request ID: the actor is root")
			return "", false
		}
		return "root", true
	}
	if !memberapi.ValidUserActor(actor) || !memberapi.ValidRequestID(requestID) {
		httpkit.WriteError(w, r, http.StatusBadRequest, "invalid_request", "this caller's request must name the user (actor \"user:NAME\") and carry a request ID")
		return "", false
	}
	log.Info("cluster socket: member operation for skep-api", "operation", op, "actor", actor,
		"api_request_id", requestID, "request_id", httpkit.RequestID(r.Context()))
	return actor, true
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
