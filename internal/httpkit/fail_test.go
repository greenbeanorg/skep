package httpkit

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/greenbeanorg/skep/pkg/models"
)

// A coded refusal keeps its code and gets its status; the jail disk quota
// below usage is 422 (ADR 0010 amendment w).
func TestFailDiskQuotaBelowUsage(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/api/v1/containers/x", nil)
	Fail(slog.New(slog.NewTextHandler(io.Discard, nil)), w, r, models.DiskQuotaBelowUsage("ctr-1", 100<<20, 132<<20))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("%v: %s", err, w.Body.String())
	}
	if body.Error.Code != models.CodeContainerDiskQuotaBelowUsage || body.Error.Details["referenced_bytes"] != float64(132<<20) {
		t.Fatalf("body = %s", w.Body.String())
	}
}

// An agent of another build is 503 agent_version_mismatch (ADR 0012
// answer 9), with no details.
func TestFailAgentVersionMismatch(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/system", nil)
	Fail(slog.New(slog.NewTextHandler(io.Discard, nil)), w, r,
		&models.Error{Code: models.CodeAgentVersionMismatch, Message: "skep-agent runs a and skep-api runs b"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error.Code != models.CodeAgentVersionMismatch || body.Error.Details != nil {
		t.Fatalf("body = %s", w.Body.String())
	}
}
