package api

import (
	"context"
	"net/http"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/forward"
	"github.com/greenbeanorg/skep/internal/httpkit"
)

// Receiver is the node listener's checks of a forwarded request (D24–D26):
// forward.Receiver.
type Receiver interface {
	Check(r *http.Request) (*forward.Accepted, *forward.Refusal)
}

// ActionForwardRefused is the receiver's access audit action of a refusal
// made after the envelope's signature verified (D28).
const ActionForwardRefused = "forward.refused"

// originKey carries the origin node of a forwarded request in its context.
type originKey struct{}

// originFrom returns the origin node of a forwarded request, or "".
func originFrom(ctx context.Context) string {
	o, _ := ctx.Value(originKey{}).(string)
	return o
}

// NewNodeHandler returns the node listener's handler (D19): its own mux,
// with only the routes this build forwards, each behind the receiver's
// checks; every other path is refused with no_route. It serves no login,
// no session, no user, cluster or audit route and no web interface, reads
// no cookie and no bearer token, and never forwards.
func NewNodeHandler(opts Options, recv Receiver) http.Handler {
	h := newHandler(opts) // h.fwd stays nil: a forwarded request is served here or refused
	byPath := map[string][]route{}
	var paths []string
	for _, rt := range h.routes() {
		if !rt.fwd {
			continue
		}
		if _, seen := byPath[rt.path]; !seen {
			paths = append(paths, rt.path)
		}
		byPath[rt.path] = append(byPath[rt.path], rt)
	}
	mux := http.NewServeMux()
	for _, path := range paths {
		mux.Handle(Prefix+path, h.dispatchForwarded(byPath[path], recv))
	}
	mux.HandleFunc("/", noRoute)
	var root http.Handler = mux
	root = httpkit.WithRecovery(h.log, root)
	root = httpkit.WithAccessLog(h.log, root)
	root = httpkit.WithSecurityHeaders(root)
	root = httpkit.WithRequestID(root)
	return root
}

func noRoute(w http.ResponseWriter, r *http.Request) {
	forward.WriteRefusal(w, r, forward.CodeNoRoute, "this node does not serve "+r.Method+" "+r.URL.Path+" for another node")
}

// dispatchForwarded picks the route by method; a method this build does
// not forward on the path is no_route too.
func (h *handler) dispatchForwarded(routes []route, recv Receiver) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.Method
		if method == http.MethodHead {
			method = http.MethodGet
		}
		for _, rt := range routes {
			if rt.method == method {
				h.serveForwarded(rt, recv, w, r)
				return
			}
		}
		noRoute(w, r)
	})
}

// serveForwarded runs the receiver's checks, then the same code path as a
// local request: the route's permissions with this node's copy of the
// user and role, the quorum gate with this node's quorum, the handler
// (D24–D28).
func (h *handler) serveForwarded(rt route, recv Receiver, w http.ResponseWriter, r *http.Request) {
	acc, ref := recv.Check(r)
	if ref != nil {
		if ref.Verified {
			r = r.WithContext(context.WithValue(r.Context(), originKey{}, ref.Origin))
			d := map[string]any{"code": ref.Code, "method": rt.method, "route": Prefix + rt.path}
			if ref.UserID != "" {
				d["user_id"] = ref.UserID
			}
			h.writeAudit(r, audit.AccessEntry{ActorName: audit.System, Action: ActionForwardRefused, Outcome: audit.Denied, Details: d})
		} else {
			h.log.InfoContext(r.Context(), "node listener: forwarded request refused", "code", ref.Code,
				"method", r.Method, "route", Prefix+rt.path, "remote", r.RemoteAddr)
		}
		forward.WriteRefusal(w, r, ref.Code, ref.Message)
		return
	}
	p := acc.Envelope.Payload
	id := auth.Identity{User: acc.User, Session: auth.Session{UserID: acc.User.ID, Kind: auth.ForwardedSession, CredentialEpoch: acc.User.CredentialEpoch}}
	ctx := httpkit.WithRequestIDValue(r.Context(), p.RequestID)
	ctx = context.WithValue(ctx, sourceIPKey{}, p.SourceIP)
	ctx = context.WithValue(ctx, originKey{}, p.Origin)
	ctx = context.WithValue(ctx, identityKey{}, id)
	ctx = agentproto.WithCaller(ctx, agentproto.Caller{
		ActorID: id.User.ID, ActorName: id.User.Username, RequestID: p.RequestID, SourceIP: p.SourceIP,
		IdempotencyKey: p.IdempotencyKey, OriginNode: p.Origin, Envelope: acc.Envelope.Raw,
	})
	r = r.WithContext(ctx)
	if h.quorum != nil && !h.quorum() {
		w.Header().Set(QuorumHeader, "none")
	}
	for _, perm := range rt.permissions() {
		if !id.User.Role.Allows(perm) {
			h.deny(w, r, id, rt, perm)
			return
		}
	}
	if !h.quorumGate(w, r, rt) {
		return
	}
	rt.handler(w, r)
}
