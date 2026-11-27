package api

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/peerapi"
	"github.com/greenbeanorg/skep/internal/forward"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Forwarding is what the origin needs to forward a request (step 8e-1):
// forward.Node.
type Forwarding interface {
	// PeersNow returns the last peers-read answer and whether it is
	// fresh (D14).
	PeersNow() (peerapi.PeersAnswer, bool)
	// GuestOwner asks skep-cluster which node owns a guest (D12).
	GuestOwner(ctx context.Context, id string) (peerapi.GuestOwnerAnswer, error)
	// Forward sends the request to a usable peer and maps the outcome
	// (D34, D35).
	Forward(ctx context.Context, p peerapi.Peer, req forward.Request) forward.Result
}

// ActionForwardSend is the origin's access audit action of a forwarded
// request that is not a GET (D36).
const ActionForwardSend = "forward.send"

// NodeParam is the query parameter that names another node (D30).
const NodeParam = "node"

// takesNodeParam reports whether a route accepts the node parameter in
// this build: the two forwardable node routes, the task routes (D30).
func (rt route) takesNodeParam() bool { return rt.scope == scopeNode && rt.fwd }

// listUsable reports whether the answer's peer list may be used: fresh,
// current, and from a node that is a member.
func listUsable(a peerapi.PeersAnswer, fresh bool) bool {
	return fresh && a.Current && !a.Joining && !a.Removed && !a.Stalled
}

// routeElsewhere decides where a request is served (D29–D31). It returns
// true when it has answered: forwarded, or refused (an undeclared scope,
// a node parameter where none is taken, an unknown or unusable node).
// False means the local handler serves the request.
func (h *handler) routeElsewhere(rt route, w http.ResponseWriter, r *http.Request) bool {
	if rt.scope == scopeUndeclared {
		httpkit.WriteError(w, r, http.StatusForbidden, auth.CodeForbidden, "this route does not declare where it is served; it is refused")
		return true
	}
	_, named := r.URL.Query()[NodeParam]
	if named && !rt.takesNodeParam() {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "this route takes no node parameter")
		return true
	}
	if h.fwd == nil || !rt.fwd {
		return false
	}
	a, fresh := h.fwd.PeersNow()
	switch rt.scope {
	case scopeNode:
		if !named {
			return false
		}
		ref := r.URL.Query().Get(NodeParam)
		if ref != "" && (ref == a.NodeID || ref == a.NodeName) {
			return false
		}
		if !listUsable(a, fresh) {
			h.writeForwardError(w, r, forward.Unreachable(peerapi.Peer{NodeID: ref, Name: ref}, "this node has no current peer list"))
			return true
		}
		p, ok := a.Find(ref)
		if !ok {
			httpkit.WriteError(w, r, http.StatusNotFound, httpkit.CodeNotFound, "no node is named "+ref+" in this cluster")
			return true
		}
		h.forwardTo(rt, w, r, a, p)
		return true
	case scopeGuest:
		id := r.PathValue("id")
		if !clusterstate.ValidNodeID(id) || a.NodeID == "" {
			return false
		}
		g, err := h.fwd.GuestOwner(r.Context(), id)
		if err != nil || !g.Found || g.Kind != routeKind(rt.path) || g.Owner == a.NodeID {
			return false
		}
		owner := peerapi.Peer{NodeID: g.Owner, Name: g.OwnerName}
		if !listUsable(a, fresh) {
			h.writeForwardError(w, r, forward.Unreachable(owner, "this node has no current peer list"))
			return true
		}
		if p, ok := a.Find(g.Owner); ok {
			owner = p
		}
		h.forwardTo(rt, w, r, a, owner)
		return true
	}
	return false
}

// routeKind is the guest kind a guest route's path names.
func routeKind(path string) string {
	if strings.HasPrefix(path, "/containers/") {
		return clusterstate.KindContainer
	}
	return clusterstate.KindVM
}

// forwardTo forwards the request to p when p is usable, refusing it with
// 503 node_unreachable or node_clock_skew otherwise, nothing sent.
func (h *handler) forwardTo(rt route, w http.ResponseWriter, r *http.Request, a peerapi.PeersAnswer, p peerapi.Peer) {
	if !peerapi.Usable(p, a.NodeID) {
		why := peerapi.WhyNotUsable(p, a.NodeID)
		if why == "" || p.State == "" {
			why = "it is not in this node's peer list"
		}
		h.finishForward(rt, w, r, p, forward.Result{Err: forward.Unreachable(p, why)})
		return
	}
	if e := forward.ClockSkew(p); e != nil {
		h.finishForward(rt, w, r, p, forward.Result{Err: e})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, forward.MaxBody+1))
	if err != nil || len(body) > forward.MaxBody {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "the request body is larger than 64 KiB or cannot be read; nothing was sent")
		return
	}
	id, _ := identityFrom(r.Context())
	res := h.fwd.Forward(r.Context(), p, forward.Request{
		Method: r.Method, URI: r.URL.RequestURI(), Body: body, ContentType: r.Header.Get("Content-Type"),
		IdempotencyKey: r.Header.Get(agentproto.HeaderIdempotencyKey), RequestID: httpkit.RequestID(r.Context()),
		SourceIP: sourceIPFrom(r.Context()), UserID: id.User.ID, Epoch: id.User.CredentialEpoch,
	})
	h.finishForward(rt, w, r, p, res)
}

// finishForward answers a forward's result and audits it (D34–D36).
func (h *handler) finishForward(rt route, w http.ResponseWriter, r *http.Request, p peerapi.Peer, res forward.Result) {
	if !safeMethod(r.Method) {
		id, _ := identityFrom(r.Context())
		e := audit.AccessEntry{ActorID: id.User.ID, ActorName: id.User.Username, Action: ActionForwardSend, Outcome: audit.Success,
			Details: map[string]any{"target_node": p.NodeID, "target_name": p.Name, "method": r.Method, "route": Prefix + rt.path, "status": res.HTTPStatus()}}
		if res.Err != nil {
			e.Outcome = audit.Failure
			e.Details["error_code"] = res.Err.Code
			if o, ok := res.Err.Details["outcome"]; ok {
				e.Details["outcome"] = o
			}
		} else if res.Status >= 400 {
			e.Outcome = audit.Failure
		}
		h.writeAudit(r, e)
	}
	if res.Err != nil {
		h.writeForwardError(w, r, res.Err)
		return
	}
	for _, k := range []string{"Content-Type", "Location", "Retry-After"} {
		if v := res.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set(models.NodeHeader, p.NodeID)
	w.WriteHeader(res.Status)
	_, _ = w.Write(res.Body)
}

// writeForwardError answers one of the four public codes, 400 for an
// envelope that would be too large, or 401 for an origin whose copy of
// the user is behind (stale_origin).
func (h *handler) writeForwardError(w http.ResponseWriter, r *http.Request, e *models.Error) {
	if e.Code == auth.CodeUnauthenticated {
		h.fail(w, r, auth.ErrUnauthenticated)
		return
	}
	httpkit.WriteErrorDetails(w, r, forward.Result{Err: e}.HTTPStatus(), e.Code, e.Message, e.Details)
}
