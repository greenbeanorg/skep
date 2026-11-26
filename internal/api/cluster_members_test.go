package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/cluster/joincode"
	"github.com/greenbeanorg/skep/internal/cluster/memberapi"
	"github.com/greenbeanorg/skep/pkg/models"
)

func (e *env) clusterHandler(cb ClusterBackend) http.Handler {
	return NewHandler(Options{
		Backend: newBackend(), Auth: e.authn, Users: e.users, Audit: e.access, AuditLog: e.access,
		Logger: slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), Cluster: cb,
	})
}

// joinRequest is a well-formed request line of a host named wu.
func joinRequest(t *testing.T) string {
	t.Helper()
	line, err := joincode.EncodeRequest(joincode.Request{Node: joincode.Node{NodeID: "2d7a5c74-9b2a-4e7a-9f2d-5b3f8b2c1e33",
		Name: "wu", Address: "192.0.2.9:8444", Pin: "sha256:" + strings.Repeat("ab", 32)}})
	if err != nil {
		t.Fatal(err)
	}
	return line
}

// errorOf decodes an error answer.
func errorOf(t *testing.T, body []byte) models.ErrorBody {
	t.Helper()
	var e models.ErrorResponse
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("not an error answer: %s", body)
	}
	return e.Error
}

// lastAudit returns the newest access audit entry with the action.
func lastAudit(t *testing.T, e *env, action string) models.AuditEntry {
	t.Helper()
	entries, _, err := e.access.List(context.Background(), 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		if en.Action == action {
			return en
		}
	}
	t.Fatalf("no audit entry %s", action)
	return models.AuditEntry{}
}

// D19, D20, D26 against a real one-node skep-cluster: the view for a
// viewer; the operator refused with the permission named and audited;
// the admin's admission refused by the cluster (no listen address) with
// the command named; a removal's cluster ID required and compared.
func TestClusterThroughARealSocket(t *testing.T) {
	e := freshEnv(t)
	h := e.clusterHandler(memberapi.NewClient(e.node.Socket))
	rec := e.serve(h, request{method: http.MethodGet, path: Prefix + "/cluster", role: auth.RoleViewer})
	var view models.Cluster
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &view) != nil || view.ClusterID == "" ||
		len(view.Members) != 1 || !view.Members[0].Voter || !view.LeaderKnown || view.Health.Status == "" {
		t.Fatalf("view: %d %s", rec.Code, rec.Body)
	}
	body := `{"request":"` + joinRequest(t) + `"}`
	rec = e.serve(h, request{method: http.MethodPost, path: Prefix + "/cluster/admissions", role: auth.RoleOperator, body: body})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), auth.PermClusterManage) {
		t.Fatalf("operator: %d %s", rec.Code, rec.Body)
	}
	if en := lastAudit(t, e, auth.ActionDenied); en.Details["permission"] != auth.PermClusterManage {
		t.Fatalf("denial audit: %+v", en)
	}
	rec = e.serve(h, request{method: http.MethodPost, path: Prefix + "/cluster/admissions", role: auth.RoleAdmin, body: body})
	if eb := errorOf(t, rec.Body.Bytes()); rec.Code != http.StatusConflict || eb.Code != CodeClusterRefused || !strings.Contains(eb.Message, "skep-cluster listen -address ADDR") {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body)
	}
	if en := lastAudit(t, e, ActionClusterAdmit); en.Outcome != "failure" || en.Details["error_code"] != CodeClusterRefused || en.Details["node_name"] != "wu" {
		t.Fatalf("admit audit: %+v", en)
	}
	path := Prefix + "/cluster/members/test/remove"
	for _, c := range []struct {
		body string
		code int
		want string
	}{
		{`{}`, http.StatusBadRequest, "cluster_id is required"},
		{`{"cluster_id":"` + view.ClusterID + `","allow_unanswered":true}`, http.StatusBadRequest, "not the expected JSON"},
		{`{"cluster_id":"00000000-0000-4000-8000-000000000009"}`, http.StatusConflict, CodeClusterIDMismatch},
	} {
		rec := e.serve(h, request{method: http.MethodPost, path: path, role: auth.RoleAdmin, body: c.body})
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: %d %s", c.body, rec.Code, rec.Body)
		}
	}
	// A bad name is refused before the socket.
	if rec := e.serve(h, request{method: http.MethodDelete, path: Prefix + "/cluster/admissions/bad%20name", role: auth.RoleAdmin}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad name: %d", rec.Code)
	}
}

// fakeMembers records what skep-api sends and answers err.
type fakeMembers struct {
	err     error
	actor   string
	request string
	removed memberapi.RemoveRequest
}

func (f *fakeMembers) Status(context.Context) (memberapi.View, error) { return memberapi.View{}, f.err }
func (f *fakeMembers) Health(context.Context) (models.Health, error) {
	return models.Health{Status: models.HealthOK}, nil
}
func (f *fakeMembers) Admit(_ context.Context, req memberapi.AdmitRequest) (memberapi.AdmitResult, error) {
	f.actor, f.request = req.Actor, req.RequestID
	return memberapi.AdmitResult{Name: "wu", NodeID: "n9", OpID: "op1"}, f.err
}
func (f *fakeMembers) Withdraw(_ context.Context, req memberapi.WithdrawRequest) (memberapi.WithdrawResult, error) {
	f.actor = req.Actor
	return memberapi.WithdrawResult{Name: req.Name, NodeID: "n9", OpID: "op2"}, f.err
}
func (f *fakeMembers) Remove(_ context.Context, req memberapi.RemoveRequest) (memberapi.RemoveResult, error) {
	f.actor, f.removed = req.Actor, req
	return memberapi.RemoveResult{Name: req.Name, NodeID: "n9", OpID: "op3", ClusterID: req.ClusterID, HandedOver: true}, f.err
}

// D25: every answer of the socket has its public error; skep-api sends
// each operation once, with the user as the actor and a request ID (D22).
func TestClusterErrorMapping(t *testing.T) {
	e := freshEnv(t)
	sock := func(status int, code, msg string) error {
		return &memberapi.Error{HTTPStatus: status, Code: code, Message: msg}
	}
	for _, c := range []struct {
		err     error
		status  int
		code    string
		outcome string
		lacks   string
	}{
		{memberapi.ErrUnreachable, 503, auth.CodeNoQuorum, "not_written", ""},
		{memberapi.ErrNoAnswer, 503, auth.CodeNoQuorum, "unknown", ""},
		{sock(400, "invalid_request", "bad"), 400, "invalid_request", "", ""},
		{sock(404, "not_found", "no such"), 404, "not_found", "", ""},
		{sock(409, "refused", "being removed"), 409, CodeClusterRefused, "", ""},
		{sock(409, "rotation_in_progress", "rotating"), 409, CodeClusterRefused, "", ""},
		{sock(409, "voters_unanswered", "run the command again with -allow-unanswered"), 409, CodeVotersUnanswered, "", "allow-unanswered"},
		{sock(409, "cluster_id_mismatch", "x"), 409, CodeClusterIDMismatch, "", ""},
		{sock(503, "no_leader", "x"), 503, auth.CodeNoQuorum, "not_written", ""},
		{sock(503, "unavailable", "x"), 503, auth.CodeNoQuorum, "not_written", ""},
		{sock(503, "handover_failed", "x"), 503, auth.CodeNoQuorum, "not_written", ""},
		{sock(409, "joining", "x"), 503, auth.CodeNoQuorum, "not_written", ""},
		{sock(409, "removed", "x"), 503, auth.CodeNoQuorum, "not_written", ""},
		{sock(403, "forbidden", "x"), 503, auth.CodeNoQuorum, "not_written", ""},
		{sock(503, "outcome_unknown", "x"), 503, auth.CodeNoQuorum, "unknown", ""},
		{sock(503, "not_committed", "x"), 503, auth.CodeNoQuorum, "unknown", ""},
	} {
		f := &fakeMembers{err: c.err}
		h := e.clusterHandler(f)
		rec := e.serve(h, request{method: http.MethodPost, path: Prefix + "/cluster/members/wu/remove", role: auth.RoleAdmin,
			body: `{"cluster_id":"00000000-0000-4000-8000-000000000009"}`})
		eb := errorOf(t, rec.Body.Bytes())
		if rec.Code != c.status || eb.Code != c.code || (c.outcome != "" && eb.Details["outcome"] != c.outcome) ||
			(c.lacks != "" && strings.Contains(eb.Message, c.lacks)) {
			t.Errorf("%v: %d %s", c.err, rec.Code, rec.Body)
		}
	}
	f := &fakeMembers{}
	h := e.clusterHandler(f)
	rec := e.serve(h, request{method: http.MethodPost, path: Prefix + "/cluster/members/wu/remove", role: auth.RoleAdmin,
		body: `{"cluster_id":"00000000-0000-4000-8000-000000000009"}`})
	var rm models.ClusterRemoval
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &rm) != nil || !rm.HandedOver || f.removed.AllowUnanswered || f.removed.ClusterID == "" {
		t.Fatalf("remove: %d %s %+v", rec.Code, rec.Body, f.removed)
	}
	if !strings.HasPrefix(f.actor, "user:") || len(f.actor) < 6 {
		t.Fatalf("actor %q", f.actor)
	}
	if en := lastAudit(t, e, ActionClusterRemove); en.Outcome != "success" || en.Details["op_id"] != "op3" || en.ResourceID != "n9" {
		t.Fatalf("remove audit: %+v", en)
	}
	rec = e.serve(h, request{method: http.MethodPost, path: Prefix + "/cluster/admissions", role: auth.RoleAdmin, body: `{"request":"` + joinRequest(t) + `"}`})
	var adm models.ClusterAdmission
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &adm) != nil || adm.Address != "192.0.2.9:8444" || f.request == "" {
		t.Fatalf("admit: %d %s (request id %q)", rec.Code, rec.Body, f.request)
	}
	rec = e.serve(h, request{method: http.MethodPost, path: Prefix + "/cluster/admissions", role: auth.RoleAdmin, body: `{"request":"` + strings.Repeat("A", 2049) + `"}`})
	if rec.Code != 400 {
		t.Fatalf("long line: %d", rec.Code)
	}
	if rec := e.serve(h, request{method: http.MethodDelete, path: Prefix + "/cluster/admissions/wu", role: auth.RoleAdmin}); rec.Code != 200 {
		t.Fatalf("withdraw: %d %s", rec.Code, rec.Body)
	}
	if en := lastAudit(t, e, ActionClusterWithdraw); en.Details["node_name"] != "wu" || en.Details["op_id"] != "op2" {
		t.Fatalf("withdraw audit: %+v", en)
	}
	// The view without a socket: 503 no_quorum.
	if rec := e.serve(e.clusterHandler(&fakeMembers{err: memberapi.ErrUnreachable}), request{method: http.MethodGet, path: Prefix + "/cluster", role: auth.RoleAdmin}); rec.Code != 503 || !strings.Contains(rec.Body.String(), auth.CodeNoQuorum) {
		t.Fatalf("view without a socket: %d %s", rec.Code, rec.Body)
	}
}

// D19: only the admin role has cluster:manage.
func TestClusterManageOnlyForAdmin(t *testing.T) {
	e := freshEnv(t)
	roles, err := e.users.Roles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range roles {
		if got := r.Allows(auth.PermClusterManage); got != (r.Name == auth.RoleAdmin) {
			t.Errorf("%s: cluster:manage %v", r.Name, got)
		}
	}
}
