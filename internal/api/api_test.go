package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

type fakeBackend struct {
	health    models.Health
	system    models.System
	systemErr error
	hardware  models.Hardware
	hwErr     error
	panicMsg  string
	caller    agentproto.Caller
	cancelled []string
	cancelErr error
	tasks     models.TaskList
	tasksErr  error
	taskOpts  models.TaskListOptions
	task      models.TaskDetail
}

func (f *fakeBackend) Tasks(_ context.Context, opts models.TaskListOptions) (models.TaskList, error) {
	f.taskOpts = opts
	return f.tasks, f.tasksErr
}

func (f *fakeBackend) Task(_ context.Context, id string) (models.TaskDetail, error) {
	if id != f.task.ID {
		return models.TaskDetail{}, fmt.Errorf("task %q: %w", id, models.ErrNotFound)
	}
	return f.task, nil
}

func (f *fakeBackend) CancelTask(ctx context.Context, id string) (models.Task, error) {
	f.caller, _ = agentproto.CallerFrom(ctx)
	f.cancelled = append(f.cancelled, id)
	if f.cancelErr != nil {
		return models.Task{}, f.cancelErr
	}
	if id != f.task.ID {
		return models.Task{}, fmt.Errorf("task %q: %w", id, models.ErrNotFound)
	}
	t := f.task.Task
	t.Status = models.TaskCancelled
	return t, nil
}

func (f *fakeBackend) Health(context.Context) models.Health { return f.health }

func (f *fakeBackend) System(ctx context.Context) (models.System, error) {
	f.caller, _ = agentproto.CallerFrom(ctx)
	if f.panicMsg != "" {
		panic(f.panicMsg)
	}
	return f.system, f.systemErr
}

func (f *fakeBackend) Hardware(_ context.Context, id string) (models.Hardware, error) {
	if f.hwErr != nil {
		return models.Hardware{}, f.hwErr
	}
	if id != f.hardware.NodeID {
		return models.Hardware{}, fmt.Errorf("node %q: %w", id, models.ErrNotFound)
	}
	return f.hardware, nil
}

func newBackend() *fakeBackend {
	return &fakeBackend{
		health: models.Health{Status: models.HealthOK, Version: "test", Checks: map[string]models.HealthCheck{}},
		system: models.System{NodeID: "node-1", Hostname: "hearst", Platform: models.PlatformFreeBSD},
		hardware: models.Hardware{
			NodeID:   "node-1",
			Platform: models.PlatformFreeBSD,
			CPU:      models.CPUInfo{Model: "Test CPU", Cores: 2, Threads: 4},
			Warnings: []string{},
		},
		tasks: models.TaskList{
			Items: []models.Task{{
				ID: "task-1", Type: "discovery.refresh", Status: models.TaskSucceeded,
				Actor: models.TaskActor{ID: "system", Name: "system"},
			}},
			NextCursor: "next-page",
		},
		task: models.TaskDetail{
			Task: models.Task{
				ID: "task-1", Type: "discovery.refresh", Status: models.TaskFailed,
				Actor: models.TaskActor{ID: "system", Name: "system"},
				Error: &models.TaskError{Code: "timeout", Message: "the task exceeded its time limit"},
			},
			Events: []models.TaskEvent{{ID: 1, Kind: models.TaskEventState, Level: "info", Message: "task is pending"}},
		},
	}
}

// do calls the API as the test administrator.
func do(t *testing.T, b Backend, method, path string, logBuf *bytes.Buffer) *httptest.ResponseRecorder {
	t.Helper()
	e := shared(t)
	return e.serve(e.handler(b, logBuf), request{method: method, path: path, role: auth.RoleAdmin})
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	var v T
	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("response is not the expected JSON: %v", err)
	}
	return v
}

func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) models.ErrorBody {
	t.Helper()
	if rec.Code != status {
		t.Errorf("status = %d, want %d", rec.Code, status)
	}
	body := decode[models.ErrorResponse](t, rec).Error
	if body.Code != code {
		t.Errorf("error code = %q, want %q", body.Code, code)
	}
	if body.Message == "" {
		t.Error("error message is empty")
	}
	if body.RequestID == "" || body.RequestID != rec.Header().Get(httpkit.RequestIDHeader) {
		t.Errorf("request_id %q does not match the %s header %q", body.RequestID, httpkit.RequestIDHeader, rec.Header().Get(httpkit.RequestIDHeader))
	}
	return body
}

func TestHealth(t *testing.T) {
	rec := do(t, newBackend(), http.MethodGet, "/api/v1/health", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := decode[models.Health](t, rec); got.Status != models.HealthOK || got.Version != "test" {
		t.Errorf("health = %+v", got)
	}
}

// TestHealthStatusCodes: only an unavailable service answers 503. A
// degraded one keeps serving and answers 200, saying so in the body.
func TestHealthStatusCodes(t *testing.T) {
	for _, tc := range []struct {
		status string
		code   int
	}{
		{models.HealthOK, http.StatusOK},
		{"", http.StatusOK},
		{models.HealthDegraded, http.StatusOK},
		{models.HealthUnavailable, http.StatusServiceUnavailable},
	} {
		b := newBackend()
		b.health.Status = tc.status
		rec := do(t, b, http.MethodGet, "/api/v1/health", nil)
		if rec.Code != tc.code {
			t.Errorf("%q: status = %d, want %d", tc.status, rec.Code, tc.code)
		}
		want := tc.status
		if want == "" {
			want = models.HealthOK
		}
		if got := decode[models.Health](t, rec); got.Status != want {
			t.Errorf("%q: health = %+v", tc.status, got)
		}
	}
}

func TestSystem(t *testing.T) {
	rec := do(t, newBackend(), http.MethodGet, "/api/v1/system", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := decode[models.System](t, rec); got.NodeID != "node-1" || got.Hostname != "hearst" {
		t.Errorf("system = %+v", got)
	}
}

func TestNodeHardware(t *testing.T) {
	rec := do(t, newBackend(), http.MethodGet, "/api/v1/nodes/node-1/hardware", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if got := decode[models.Hardware](t, rec); got.NodeID != "node-1" || got.CPU.Threads != 4 {
		t.Errorf("hardware = %+v", got)
	}
}

func TestNodeHardwareErrors(t *testing.T) {
	wantError(t, do(t, newBackend(), http.MethodGet, "/api/v1/nodes/other/hardware", nil),
		http.StatusNotFound, httpkit.CodeNotFound)
	wantError(t, do(t, newBackend(), http.MethodGet, "/api/v1/nodes/bad%20id/hardware", nil),
		http.StatusBadRequest, httpkit.CodeInvalidRequest)
	wantError(t, do(t, newBackend(), http.MethodGet, "/api/v1/nodes/"+strings.Repeat("a", 65)+"/hardware", nil),
		http.StatusBadRequest, httpkit.CodeInvalidRequest)
}

func TestInternalErrorsAreNotLeaked(t *testing.T) {
	var logs bytes.Buffer
	b := newBackend()
	b.systemErr = errors.New("open /var/db/skep/agent/agent.db: permission denied")

	rec := do(t, b, http.MethodGet, "/api/v1/system", &logs)
	body := wantError(t, rec, http.StatusInternalServerError, httpkit.CodeInternal)
	if strings.Contains(body.Message, "agent.db") {
		t.Errorf("internal error text leaked to the client: %q", body.Message)
	}
	if !strings.Contains(logs.String(), "permission denied") {
		t.Error("the internal error must be logged")
	}
}

func TestCancelledRequestIs503(t *testing.T) {
	b := newBackend()
	b.hwErr = fmt.Errorf("agent: host discovery: %w", context.DeadlineExceeded)
	wantError(t, do(t, b, http.MethodGet, "/api/v1/nodes/node-1/hardware", nil),
		http.StatusServiceUnavailable, httpkit.CodeUnavailable)
}

func TestPanicBecomesErrorEnvelope(t *testing.T) {
	var logs bytes.Buffer
	b := newBackend()
	b.panicMsg = "boom"
	wantError(t, do(t, b, http.MethodGet, "/api/v1/system", &logs), http.StatusInternalServerError, httpkit.CodeInternal)
	if !strings.Contains(logs.String(), "boom") {
		t.Error("the panic must be logged")
	}
}

func TestUnknownPathsAndMethods(t *testing.T) {
	for _, path := range []string{"/", "/api/v1", "/api/v1/nope", "/api/v2/health", "/api/v1/nodes/node-1"} {
		wantError(t, do(t, newBackend(), http.MethodGet, path, nil), http.StatusNotFound, httpkit.CodeNotFound)
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		for _, path := range []string{"/api/v1/health", "/api/v1/system", "/api/v1/nodes/node-1/hardware"} {
			rec := do(t, newBackend(), method, path, nil)
			wantError(t, rec, http.StatusMethodNotAllowed, httpkit.CodeMethodNotAllowed)
			if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
				t.Errorf("%s %s: Allow = %q", method, path, allow)
			}
		}
	}
}

func TestHeadHasNoBody(t *testing.T) {
	rec := do(t, newBackend(), http.MethodHead, "/api/v1/health", nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("HEAD: status = %d, body = %q", rec.Code, rec.Body)
	}
}

func TestRequestIDAndSecurityHeaders(t *testing.T) {
	h := shared(t).handler(newBackend(), nil)

	send := func(id string) http.Header {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
		if id != "" {
			req.Header.Set(httpkit.RequestIDHeader, id)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Header()
	}

	if got := send("client-id_1.2").Get(httpkit.RequestIDHeader); got != "client-id_1.2" {
		t.Errorf("a well-formed client request ID must be kept, got %q", got)
	}
	for _, bad := range []string{"has space", "new\nline", strings.Repeat("x", 65)} {
		if got := send(bad).Get(httpkit.RequestIDHeader); got == bad || got == "" {
			t.Errorf("malformed request ID %q must be replaced, got %q", bad, got)
		}
	}
	hdr := send("")
	if len(hdr.Get(httpkit.RequestIDHeader)) != 24 {
		t.Errorf("generated request ID = %q", hdr.Get(httpkit.RequestIDHeader))
	}
	if hdr.Get("X-Content-Type-Options") != "nosniff" || hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("security headers missing: %v", hdr)
	}
}

func TestAccessLog(t *testing.T) {
	var logs bytes.Buffer
	do(t, newBackend(), http.MethodGet, "/api/v1/nodes/other/hardware", &logs)

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &rec); err != nil {
		t.Fatalf("access log is not one JSON line: %v\n%s", err, logs.String())
	}
	if rec["msg"] != "http request" || rec["method"] != "GET" || rec["status"] != float64(404) ||
		rec["path"] != "/api/v1/nodes/other/hardware" || rec["request_id"] == "" {
		t.Errorf("access log = %v", rec)
	}
}

func TestTasksList(t *testing.T) {
	b := newBackend()
	rec := do(t, b, http.MethodGet, "/api/v1/tasks?status=succeeded&limit=25&cursor=abc", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	got := decode[models.TaskList](t, rec)
	if len(got.Items) != 1 || got.Items[0].ID != "task-1" || got.NextCursor != "next-page" {
		t.Errorf("list = %+v", got)
	}
	want := models.TaskListOptions{Status: models.TaskSucceeded, Limit: 25, Cursor: "abc"}
	if b.taskOpts != want {
		t.Errorf("options passed to the backend = %+v, want %+v", b.taskOpts, want)
	}

	// No parameters: the backend picks the defaults.
	do(t, b, http.MethodGet, "/api/v1/tasks", nil)
	if b.taskOpts != (models.TaskListOptions{}) {
		t.Errorf("options = %+v, want zero values", b.taskOpts)
	}
}

func TestTasksListErrors(t *testing.T) {
	for _, q := range []string{"limit=abc", "limit=0", "limit=-5"} {
		wantError(t, do(t, newBackend(), http.MethodGet, "/api/v1/tasks?"+q, nil),
			http.StatusBadRequest, httpkit.CodeInvalidRequest)
	}
	// Validation errors from the service keep their message.
	b := newBackend()
	b.tasksErr = models.Invalid("unknown task status done")
	body := wantError(t, do(t, b, http.MethodGet, "/api/v1/tasks?status=done", nil),
		http.StatusBadRequest, httpkit.CodeInvalidRequest)
	if body.Message != "unknown task status done" {
		t.Errorf("message = %q", body.Message)
	}
}

func TestTaskDetail(t *testing.T) {
	rec := do(t, newBackend(), http.MethodGet, "/api/v1/tasks/task-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	got := decode[models.TaskDetail](t, rec)
	if got.ID != "task-1" || got.Error == nil || got.Error.Code != "timeout" || len(got.Events) != 1 {
		t.Errorf("task = %+v", got)
	}
	wantError(t, do(t, newBackend(), http.MethodGet, "/api/v1/tasks/other", nil), http.StatusNotFound, httpkit.CodeNotFound)
	wantError(t, do(t, newBackend(), http.MethodGet, "/api/v1/tasks/bad%20id", nil), http.StatusBadRequest, httpkit.CodeInvalidRequest)
}

// TestTasksCannotBeSubmitted: cancelling is the only thing the API can do
// to a task. Nothing can be submitted, replaced, or deleted.
func TestTasksCannotBeSubmitted(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, path := range []string{"/api/v1/tasks", "/api/v1/tasks/task-1"} {
			wantError(t, do(t, newBackend(), method, path, nil), http.StatusMethodNotAllowed, httpkit.CodeMethodNotAllowed)
		}
	}
	for _, path := range []string{"/api/v1/tasks/task-1/retry", "/api/v1/tasks/task-1/cancel/now"} {
		wantError(t, do(t, newBackend(), http.MethodPost, path, nil), http.StatusNotFound, httpkit.CodeNotFound)
	}
}

// TestDomainErrorMapping checks the HTTP status of each stable error code,
// including the conflicts the task engine returns.
func TestDomainErrorMapping(t *testing.T) {
	tests := map[string]int{
		models.CodeInvalidRequest:      http.StatusBadRequest,
		models.CodeResourceBusy:        http.StatusConflict,
		models.CodeIdempotencyConflict: http.StatusConflict,
		models.CodeNotCancellable:      http.StatusConflict,
		models.CodeQueueFull:           http.StatusServiceUnavailable,
		models.CodeShuttingDown:        http.StatusServiceUnavailable,
	}
	for code, status := range tests {
		b := newBackend()
		b.tasksErr = fmt.Errorf("wrapped: %w", &models.Error{
			Code: code, Message: "message for " + code, Details: map[string]any{"task_id": "task-9"},
		})
		body := wantError(t, do(t, b, http.MethodGet, "/api/v1/tasks", nil), status, code)
		if body.Message != "message for "+code || body.Details["task_id"] != "task-9" {
			t.Errorf("%s: body = %+v", code, body)
		}
	}

	// An unknown code is not trusted: it becomes a generic internal error.
	b := newBackend()
	b.tasksErr = &models.Error{Code: "something_new", Message: "internal detail"}
	body := wantError(t, do(t, b, http.MethodGet, "/api/v1/tasks", nil), http.StatusInternalServerError, httpkit.CodeInternal)
	if strings.Contains(body.Message, "internal detail") {
		t.Errorf("message leaked: %q", body.Message)
	}
}

// TestBackendCallsCarryACaller checks that every backend call is made for
// the authenticated user, with the request ID and the client's address:
// the agent refuses anything without an actor and records this one.
func TestBackendCallsCarryACaller(t *testing.T) {
	e := shared(t)
	b := newBackend()
	rec := e.serve(e.handler(b, nil), request{
		method: http.MethodGet, path: "/api/v1/system", role: auth.RoleOperator,
		headers: map[string]string{httpkit.RequestIDHeader: "req-7", "X-Forwarded-For": "203.0.113.9"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// X-Forwarded-For is ignored: no proxy is trusted.
	want := agentproto.Caller{ActorID: e.ids[auth.RoleOperator], ActorName: "test-operator", RequestID: "req-7", SourceIP: "192.0.2.7"}
	if b.caller != want {
		t.Errorf("caller = %+v, want %+v", b.caller, want)
	}
	if err := b.caller.Validate(); err != nil {
		t.Errorf("the caller would be refused by the agent: %v", err)
	}
}

func TestAgentUnavailableIs503(t *testing.T) {
	b := newBackend()
	b.systemErr = fmt.Errorf("wrapped: %w", &models.Error{Code: models.CodeAgentUnavailable, Message: "the host agent is not reachable"})
	wantError(t, do(t, b, http.MethodGet, "/api/v1/system", nil), http.StatusServiceUnavailable, models.CodeAgentUnavailable)
}

// TestHSTSOnlyWithOperatorCertificate: a browser that has stored the header
// cannot click through a certificate warning, so it must never be sent
// with the self-signed certificate.
func TestHSTSOnlyWithOperatorCertificate(t *testing.T) {
	e := shared(t)
	expiry := time.Date(2027, 12, 14, 0, 0, 0, 0, time.UTC)
	for source, want := range map[string]string{"self-signed": "", "operator": "max-age=63072000"} {
		info := models.TLSInfo{Source: source, NotAfter: expiry, PublicKeySHA256: "sha256:abc"}
		h := NewHandler(Options{
			Backend: newBackend(), Auth: e.authn, Users: e.users, Audit: e.access, AuditLog: e.access,
			Logger: slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
			TLS:    func() models.TLSInfo { return info },
		})
		for _, path := range []string{"/api/v1/system", "/api/v1/health", "/nope"} {
			rec := e.serve(h, request{method: http.MethodGet, path: path})
			if got := rec.Header().Get("Strict-Transport-Security"); got != want {
				t.Errorf("%s certificate, %s: Strict-Transport-Security = %q, want %q", source, path, got, want)
			}
		}
		// The system document reports the certificate, including its expiry.
		sys := decode[models.System](t, e.serve(h, request{method: http.MethodGet, path: "/api/v1/system", role: auth.RoleViewer}))
		if sys.API == nil || sys.API.TLS != info {
			t.Errorf("%s: system.api = %+v", source, sys.API)
		}
	}
	// Without TLS information (not served over TLS) neither appears.
	rec := do(t, newBackend(), http.MethodGet, "/api/v1/system", nil)
	if rec.Header().Get("Strict-Transport-Security") != "" || decode[models.System](t, rec).API != nil {
		t.Error("TLS details reported without TLS")
	}
}
