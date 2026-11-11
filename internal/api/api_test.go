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

	"github.com/greenbeanorg/skep/pkg/models"
)

type fakeBackend struct {
	health    models.Health
	system    models.System
	systemErr error
	hardware  models.Hardware
	hwErr     error
	panicMsg  string
}

func (f *fakeBackend) Health(context.Context) models.Health { return f.health }

func (f *fakeBackend) System(context.Context) (models.System, error) {
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
	}
}

func do(t *testing.T, b Backend, method, path string, logBuf *bytes.Buffer) *httptest.ResponseRecorder {
	t.Helper()
	if logBuf == nil {
		logBuf = &bytes.Buffer{}
	}
	h := NewHandler(b, slog.New(slog.NewJSONHandler(logBuf, nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
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
	if body.RequestID == "" || body.RequestID != rec.Header().Get(RequestIDHeader) {
		t.Errorf("request_id %q does not match the %s header %q", body.RequestID, RequestIDHeader, rec.Header().Get(RequestIDHeader))
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

func TestHealthDegradedIs503(t *testing.T) {
	b := newBackend()
	b.health.Status = models.HealthDegraded
	rec := do(t, b, http.MethodGet, "/api/v1/health", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if got := decode[models.Health](t, rec); got.Status != models.HealthDegraded {
		t.Errorf("health = %+v", got)
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
		http.StatusNotFound, CodeNotFound)
	wantError(t, do(t, newBackend(), http.MethodGet, "/api/v1/nodes/bad%20id/hardware", nil),
		http.StatusBadRequest, CodeInvalidRequest)
	wantError(t, do(t, newBackend(), http.MethodGet, "/api/v1/nodes/"+strings.Repeat("a", 65)+"/hardware", nil),
		http.StatusBadRequest, CodeInvalidRequest)
}

func TestInternalErrorsAreNotLeaked(t *testing.T) {
	var logs bytes.Buffer
	b := newBackend()
	b.systemErr = errors.New("open /var/db/skep/skep.db: permission denied")

	rec := do(t, b, http.MethodGet, "/api/v1/system", &logs)
	body := wantError(t, rec, http.StatusInternalServerError, CodeInternal)
	if strings.Contains(body.Message, "skep.db") {
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
		http.StatusServiceUnavailable, CodeUnavailable)
}

func TestPanicBecomesErrorEnvelope(t *testing.T) {
	var logs bytes.Buffer
	b := newBackend()
	b.panicMsg = "boom"
	wantError(t, do(t, b, http.MethodGet, "/api/v1/system", &logs), http.StatusInternalServerError, CodeInternal)
	if !strings.Contains(logs.String(), "boom") {
		t.Error("the panic must be logged")
	}
}

func TestUnknownPathsAndMethods(t *testing.T) {
	for _, path := range []string{"/", "/api/v1", "/api/v1/nope", "/api/v2/health", "/api/v1/nodes/node-1"} {
		wantError(t, do(t, newBackend(), http.MethodGet, path, nil), http.StatusNotFound, CodeNotFound)
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		for _, path := range []string{"/api/v1/health", "/api/v1/system", "/api/v1/nodes/node-1/hardware"} {
			rec := do(t, newBackend(), method, path, nil)
			wantError(t, rec, http.StatusMethodNotAllowed, CodeMethodNotAllowed)
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
	h := NewHandler(newBackend(), slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))

	send := func(id string) http.Header {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
		if id != "" {
			req.Header.Set(RequestIDHeader, id)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Header()
	}

	if got := send("client-id_1.2").Get(RequestIDHeader); got != "client-id_1.2" {
		t.Errorf("a well-formed client request ID must be kept, got %q", got)
	}
	for _, bad := range []string{"has space", "new\nline", strings.Repeat("x", 65)} {
		if got := send(bad).Get(RequestIDHeader); got == bad || got == "" {
			t.Errorf("malformed request ID %q must be replaced, got %q", bad, got)
		}
	}
	hdr := send("")
	if len(hdr.Get(RequestIDHeader)) != 24 {
		t.Errorf("generated request ID = %q", hdr.Get(RequestIDHeader))
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
