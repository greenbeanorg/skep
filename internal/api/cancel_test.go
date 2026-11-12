package api

import (
	"net/http"
	"testing"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

func TestCancelTask(t *testing.T) {
	e := freshEnv(t)
	b := newBackend()
	h := e.handler(b, nil)
	const path = Prefix + "/tasks/task-1/cancel"

	// A viewer may read tasks, not cancel them. Nothing reaches the agent.
	wantError(t, e.serve(h, request{method: http.MethodPost, path: path, role: auth.RoleViewer}), http.StatusForbidden, auth.CodeForbidden)
	wantError(t, e.serve(h, request{method: http.MethodPost, path: path}), http.StatusUnauthorized, auth.CodeUnauthenticated)
	if len(b.cancelled) != 0 {
		t.Fatalf("a refused cancel reached the backend: %v", b.cancelled)
	}

	// An operator may. The agent is told who asked.
	rec := e.serve(h, request{method: http.MethodPost, path: path, role: auth.RoleOperator,
		headers: map[string]string{httpkit.RequestIDHeader: "req-cancel"}})
	task := decode[models.Task](t, rec)
	if rec.Code != http.StatusOK || task.ID != "task-1" || task.Status != models.TaskCancelled {
		t.Fatalf("cancel: %d %+v", rec.Code, task)
	}
	want := agentproto.Caller{ActorID: e.ids[auth.RoleOperator], ActorName: "test-operator", RequestID: "req-cancel", SourceIP: "192.0.2.7"}
	if len(b.cancelled) != 1 || b.caller != want {
		t.Errorf("backend saw %v as %+v, want one cancel as %+v", b.cancelled, b.caller, want)
	}
	if n := e.auditCount(t, `action = 'task.cancel' AND outcome = 'success' AND actor_id = ? AND resource_id = 'task-1' AND request_id = 'req-cancel'`, e.ids[auth.RoleOperator]); n != 1 {
		t.Errorf("%d cancel entries in the access audit log, want 1", n)
	}

	// Errors of the agent come through with their codes.
	wantError(t, e.serve(h, request{method: http.MethodPost, path: Prefix + "/tasks/unknown/cancel", role: auth.RoleAdmin}), http.StatusNotFound, httpkit.CodeNotFound)
	b.cancelErr = &models.Error{Code: models.CodeNotCancellable, Message: "a running x.y task cannot be cancelled"}
	wantError(t, e.serve(h, request{method: http.MethodPost, path: path, role: auth.RoleAdmin}), http.StatusConflict, models.CodeNotCancellable)
	if n := e.auditCount(t, `action = 'task.cancel' AND outcome = 'failure'`); n != 2 {
		t.Errorf("%d failed cancels audited, want 2", n)
	}
	b.cancelErr = nil

	wantError(t, e.serve(h, request{method: http.MethodPost, path: Prefix + "/tasks/bad%20id/cancel", role: auth.RoleAdmin}), http.StatusBadRequest, httpkit.CodeInvalidRequest)
	wantError(t, e.serve(h, request{method: http.MethodGet, path: path, role: auth.RoleAdmin}), http.StatusMethodNotAllowed, httpkit.CodeMethodNotAllowed)

	// With a cookie session the request must come from this site.
	login := e.serve(h, request{method: http.MethodPost, path: Prefix + "/auth/login", body: loginBody("test-operator", testPassword, "")})
	c := sessionCookie(t, login)
	before := len(b.cancelled)
	wantError(t, e.serve(h, request{method: http.MethodPost, path: path, cookie: c.Value}), http.StatusForbidden, auth.CodeForbidden)
	wantError(t, e.serve(h, request{method: http.MethodPost, path: path, cookie: c.Value, headers: map[string]string{"Origin": "https://evil.example"}}), http.StatusForbidden, auth.CodeForbidden)
	if len(b.cancelled) != before {
		t.Error("a cross-site cancel reached the backend")
	}
	if rec := e.serve(h, request{method: http.MethodPost, path: path, cookie: c.Value, headers: map[string]string{"Origin": origin}}); rec.Code != http.StatusOK {
		t.Errorf("cancel with a cookie and a matching Origin: %d", rec.Code)
	}
}
