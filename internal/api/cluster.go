package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joincode"
	"github.com/greenbeanorg/skep/internal/cluster/memberapi"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// ClusterBackend is skep-cluster's socket as skep-api uses it for the
// cluster view and the member operations (step 8c-2): memberapi.Client.
type ClusterBackend interface {
	Status(ctx context.Context) (memberapi.View, error)
	Health(ctx context.Context) (models.Health, error)
	Admit(ctx context.Context, req memberapi.AdmitRequest) (memberapi.AdmitResult, error)
	Withdraw(ctx context.Context, req memberapi.WithdrawRequest) (memberapi.WithdrawResult, error)
	Remove(ctx context.Context, req memberapi.RemoveRequest) (memberapi.RemoveResult, error)
}

// The access audit actions of the member operations (D27).
const (
	ActionClusterAdmit    = "cluster.admit"
	ActionClusterWithdraw = "cluster.admit.withdraw"
	ActionClusterRemove   = "cluster.member.remove"
)

// The error codes of the member operations (D25). no_quorum is
// auth.CodeNoQuorum.
const (
	CodeClusterRefused    = "cluster_refused"
	CodeVotersUnanswered  = "voters_unanswered"
	CodeClusterIDMismatch = "cluster_id_mismatch"
)

// removeWriteLimit is how long the remove endpoint may take to answer:
// the socket's limit and time to pass the answer on. It is above
// skep-api's default write timeout (60 s, facts F5), so the endpoint
// extends its own write deadline.
const removeWriteLimit = memberapi.RemoveTimeout + 30*time.Second

func (h *handler) clusterRoutes() []route {
	return []route{
		{http.MethodGet, "/cluster", auth.PermSystemRead, nil, h.clusterView, servedWithoutQuorum},
		// The guest inventory names VMs and jails (D34).
		{http.MethodGet, "/cluster/guests", auth.PermSystemRead, []string{auth.PermVMRead, auth.PermContainerRead}, h.clusterGuests, servedWithoutQuorum},
		{http.MethodPost, "/cluster/admissions", auth.PermClusterManage, nil, h.clusterAdmit, needsQuorum},
		{http.MethodDelete, "/cluster/admissions/{name}", auth.PermClusterManage, nil, h.clusterWithdraw, needsQuorum},
		{http.MethodPost, "/cluster/members/{name}/remove", auth.PermClusterManage, nil, h.clusterRemove, needsQuorum},
	}
}

// noCluster is the backend of a handler built without one: nothing is
// reachable.
type noCluster struct{}

func (noCluster) Status(context.Context) (memberapi.View, error) {
	return memberapi.View{}, memberapi.ErrUnreachable
}
func (noCluster) Health(context.Context) (models.Health, error) {
	return models.Health{}, memberapi.ErrUnreachable
}
func (noCluster) Admit(context.Context, memberapi.AdmitRequest) (memberapi.AdmitResult, error) {
	return memberapi.AdmitResult{}, memberapi.ErrUnreachable
}
func (noCluster) Withdraw(context.Context, memberapi.WithdrawRequest) (memberapi.WithdrawResult, error) {
	return memberapi.WithdrawResult{}, memberapi.ErrUnreachable
}
func (noCluster) Remove(context.Context, memberapi.RemoveRequest) (memberapi.RemoveResult, error) {
	return memberapi.RemoveResult{}, memberapi.ErrUnreachable
}

// clean drops control and Unicode format characters from text that came
// from skep-cluster before it reaches a client or the audit.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return '?'
		}
		return r
	}, s)
}

// clusterFailure maps an error of the socket to the public error (D25).
func clusterFailure(err error) *models.Error {
	notWritten := func(msg string) *models.Error { return auth.NoQuorum(auth.OutcomeNotWritten, msg) }
	unknown := func(msg string) *models.Error { return auth.NoQuorum(auth.OutcomeUnknown, msg) }
	var e *memberapi.Error
	switch {
	case errors.Is(err, memberapi.ErrUnreachable):
		return notWritten("skep-cluster is not reachable on this node; nothing was written")
	case errors.Is(err, memberapi.ErrNoAnswer):
		return unknown("the request was sent to skep-cluster and no answer came; whether it was written is unknown (see GET /api/v1/cluster)")
	case !errors.As(err, &e):
		return unknown("skep-cluster's answer could not be read; whether it was written is unknown")
	}
	msg := clean(e.Message)
	switch e.Code {
	case "invalid_request":
		return &models.Error{Code: httpkit.CodeInvalidRequest, Message: msg}
	case "not_found":
		return &models.Error{Code: httpkit.CodeNotFound, Message: msg}
	case CodeClusterIDMismatch:
		return &models.Error{Code: CodeClusterIDMismatch, Message: msg}
	case CodeVotersUnanswered:
		silent := ""
		if names := unansweredNames(e.Message); len(names) > 0 {
			silent = " (no answer from " + strings.Join(names, ", ") + ")"
		}
		return &models.Error{Code: CodeVotersUnanswered, Message: "too few of the remaining voters answered now" + silent +
			": after this removal the cluster would accept no changes until they return. Nothing was written; bring them back and try again"}
	case "no_listen_address":
		return &models.Error{Code: CodeClusterRefused, Message: "this node has no listen address for the cluster: run \"skep-cluster listen -address ADDR\" as root on the host first; nothing was written"}
	case "no_leader", "unavailable", "handover_failed", "joining", "removed", "forbidden", "not_leader":
		return notWritten("skep-cluster cannot commit anything now (" + e.Code + "): " + msg)
	case "outcome_unknown", "not_committed":
		return unknown("skep-cluster could not say whether it was written (" + e.Code + "): " + msg)
	}
	if e.HTTPStatus >= 500 || e.HTTPStatus == 0 {
		return unknown("skep-cluster answered " + e.Code + ": " + msg)
	}
	return &models.Error{Code: CodeClusterRefused, Message: "the cluster refused (" + e.Code + "): " + msg}
}

// unansweredRE finds the voters skep-cluster's voters_unanswered message
// names; the rest of that message (its override) is not passed on.
var unansweredRE = regexp.MustCompile(`\(no answer from ([^()]*)\)`)

// unansweredNames returns the names in a voters_unanswered message that
// have the shape of a node name or ID (F7 of the 8c-2 review); anything
// else is dropped.
func unansweredNames(msg string) []string {
	m := unansweredRE.FindStringSubmatch(msg)
	if m == nil {
		return nil
	}
	var out []string
	for _, n := range strings.Split(m[1], ", ") {
		if clusterstate.ValidName(n) {
			out = append(out, n)
		}
	}
	return out
}

// statusForClusterCode maps the codes of clusterFailure to statuses.
var statusForClusterCode = map[string]int{
	httpkit.CodeInvalidRequest: http.StatusBadRequest,
	httpkit.CodeNotFound:       http.StatusNotFound,
	CodeClusterRefused:         http.StatusConflict,
	CodeVotersUnanswered:       http.StatusConflict,
	CodeClusterIDMismatch:      http.StatusConflict,
	auth.CodeNoQuorum:          http.StatusServiceUnavailable,
}

// writeClusterFailure writes e and records it in the audit entry.
func (h *handler) writeClusterFailure(w http.ResponseWriter, r *http.Request, entry audit.AccessEntry, e *models.Error) {
	entry.Outcome = audit.Failure
	if entry.Details == nil {
		entry.Details = map[string]any{}
	}
	entry.Details["error_code"] = e.Code
	if o, ok := e.Details["outcome"]; ok {
		entry.Details["outcome"] = o
	}
	h.writeAudit(r, entry)
	httpkit.WriteErrorDetails(w, r, statusForClusterCode[e.Code], e.Code, e.Message, e.Details)
}

// memberEntry is the audit entry of a member operation for the caller.
func memberEntry(r *http.Request, action, nodeID, name string) audit.AccessEntry {
	who, _ := identityFrom(r.Context())
	return audit.AccessEntry{ActorID: who.User.ID, ActorName: who.User.Username, Action: action,
		ResourceType: "cluster_member", ResourceID: nodeID, Outcome: audit.Success,
		Details: map[string]any{"node_name": name, "node_id": nodeID}}
}

// socketActor is the actor and request ID passed to the socket (D22).
func socketActor(r *http.Request) (string, string) {
	who, _ := identityFrom(r.Context())
	return "user:" + who.User.Username, httpkit.RequestID(r.Context())
}

func (h *handler) clusterView(w http.ResponseWriter, r *http.Request) {
	v, err := h.cluster.Status(r.Context())
	if err != nil {
		e := clusterFailure(err)
		if e.Code != auth.CodeNoQuorum {
			e = auth.NoQuorum(auth.OutcomeNotWritten, "skep-cluster's status cannot be read: "+e.Message)
		}
		httpkit.WriteErrorDetails(w, r, http.StatusServiceUnavailable, e.Code, e.Message, e.Details)
		return
	}
	out := models.Cluster{ClusterID: v.ClusterID, NodeID: v.NodeID, NodeName: v.NodeName, LeaderKnown: v.Raft.LeaderID != "",
		Members: []models.ClusterMember{}, Pendings: []models.ClusterPending{}, Removals: v.Removals, Rotations: v.Rotations,
		UnappliedRemoval: v.UnappliedRemoval, Build: v.Build}
	if v.StateMachine.Stalled != nil {
		out.StateMachineStoppedAt = v.StateMachine.Stalled.Index
	}
	voters := map[string]bool{}
	for _, s := range v.Servers {
		voters[s.ID] = s.Voter
	}
	for _, m := range v.Members {
		out.Members = append(out.Members, models.ClusterMember{ID: m.ID, Name: m.Name, State: m.State, Address: m.Address,
			Pin: m.Pin, NextPin: m.NextPin, Voter: voters[m.ID]})
	}
	for _, p := range v.Pendings {
		out.Pendings = append(out.Pendings, models.ClusterPending{NodeID: p.NodeID, Name: p.Name, Address: p.Address, Pin: p.Pin,
			ExpiresAt: time.Unix(p.ExpiresAt, 0).UTC()})
	}
	for _, c := range v.Clocks {
		out.Clocks = append(out.Clocks, models.ClusterClock{NodeID: c.NodeID, Name: c.Name, Known: c.Known, OffsetMillis: c.OffsetMillis,
			UncertaintyMillis: c.UncertaintyMillis, RoundTripMillis: c.RoundTripMillis, Note: c.Note})
	}
	if v.Join != nil {
		out.Joining = &models.ClusterJoining{Phase: v.Join.Phase, ClusterID: v.Join.ClusterID, Message: v.Join.Message}
	}
	if v.Removed != nil {
		out.Removed = &models.ClusterRemovedAs{ClusterID: v.Removed.ClusterID, At: v.Removed.At, How: v.Removed.How}
	}
	if hl, err := h.cluster.Health(r.Context()); err == nil {
		out.Health = hl
	} else {
		out.Health = models.Health{Status: models.HealthUnavailable}
	}
	httpkit.WriteJSON(w, r, http.StatusOK, out)
}

func (h *handler) clusterAdmit(w http.ResponseWriter, r *http.Request) {
	var req models.ClusterAdmitRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req.Request) > memberapi.MaxRequestLine {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "the request line is longer than 2 KiB")
		return
	}
	jr, err := joincode.ParseRequest(strings.TrimSpace(req.Request))
	if err != nil {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "not a join request line: "+err.Error())
		return
	}
	entry := memberEntry(r, ActionClusterAdmit, jr.NodeID, jr.Name)
	actor, requestID := socketActor(r)
	res, err := h.cluster.Admit(r.Context(), memberapi.AdmitRequest{Request: strings.TrimSpace(req.Request), Actor: actor, RequestID: requestID})
	if err != nil {
		h.writeClusterFailure(w, r, entry, clusterFailure(err))
		return
	}
	entry.Details["op_id"] = res.OpID
	h.writeAudit(r, entry)
	httpkit.WriteJSON(w, r, http.StatusOK, models.ClusterAdmission{Admission: res.Admission, ClusterID: res.ClusterID, NodeID: res.NodeID,
		Name: res.Name, Address: jr.Address, Pin: res.Pin, ExpiresAt: time.Unix(res.ExpiresAt, 0).UTC(), OpID: res.OpID})
}

// memberName reads a node name from the path.
func memberName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if !clusterstate.ValidName(name) {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "not a valid node name")
		return "", false
	}
	return name, true
}

func (h *handler) clusterWithdraw(w http.ResponseWriter, r *http.Request) {
	name, ok := memberName(w, r)
	if !ok {
		return
	}
	entry := memberEntry(r, ActionClusterWithdraw, "", name)
	actor, requestID := socketActor(r)
	res, err := h.cluster.Withdraw(r.Context(), memberapi.WithdrawRequest{Name: name, Actor: actor, RequestID: requestID})
	if err != nil {
		h.writeClusterFailure(w, r, entry, clusterFailure(err))
		return
	}
	entry.ResourceID, entry.Details["node_id"], entry.Details["op_id"] = res.NodeID, res.NodeID, res.OpID
	h.writeAudit(r, entry)
	httpkit.WriteJSON(w, r, http.StatusOK, models.ClusterWithdrawal{NodeID: res.NodeID, Name: res.Name, OpID: res.OpID})
}

func (h *handler) clusterRemove(w http.ResponseWriter, r *http.Request) {
	name, ok := memberName(w, r)
	if !ok {
		return
	}
	var req models.ClusterRemoveRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !clusterstate.ValidNodeID(req.ClusterID) {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "cluster_id is required: the ID of the cluster the member is removed from (GET /api/v1/cluster)")
		return
	}
	// The socket may take up to its own limit; the server's write
	// timeout must not cut the answer (F5).
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(removeWriteLimit))
	entry := memberEntry(r, ActionClusterRemove, "", name)
	entry.Details["cluster_id"] = req.ClusterID
	actor, requestID := socketActor(r)
	res, err := h.cluster.Remove(r.Context(), memberapi.RemoveRequest{Name: name, ClusterID: req.ClusterID, Actor: actor, RequestID: requestID})
	if err != nil {
		h.writeClusterFailure(w, r, entry, clusterFailure(err))
		return
	}
	entry.ResourceID, entry.Details["node_id"], entry.Details["op_id"], entry.Details["handed_over"] = res.NodeID, res.NodeID, res.OpID, res.HandedOver
	h.writeAudit(r, entry)
	httpkit.WriteJSON(w, r, http.StatusOK, models.ClusterRemoval{NodeID: res.NodeID, Name: res.Name, ClusterID: res.ClusterID, OpID: res.OpID, HandedOver: res.HandedOver})
}
