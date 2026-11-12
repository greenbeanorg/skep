// Package httpkit holds the HTTP plumbing shared by the public API
// (internal/api, served by skep-api) and the internal API (internal/internalapi,
// served by skep-agent on its Unix socket): the JSON error envelope, request
// IDs, the access log, panic recovery, and the mapping from service errors
// to responses.
package httpkit

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/greenbeanorg/skep/pkg/models"
)

// Stable, machine-readable error codes returned in the error envelope.
const (
	CodeInvalidRequest   = "invalid_request"
	CodeNotFound         = "not_found"
	CodeMethodNotAllowed = "method_not_allowed"
	CodeInternal         = "internal_error"
	CodeUnavailable      = "service_unavailable"
)

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.ErrorContext(r.Context(), "http: encode response", "error", err, "request_id", RequestID(r.Context()))
		status = http.StatusInternalServerError
		body = []byte(`{"error":{"code":"` + CodeInternal + `","message":"failed to encode the response"}}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(append(body, '\n'))
	}
}

// WriteError writes the standard error envelope.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	WriteErrorDetails(w, r, status, code, message, nil)
}

// WriteErrorDetails writes the standard error envelope with details.
func WriteErrorDetails(w http.ResponseWriter, r *http.Request, status int, code, message string, details map[string]any) {
	WriteJSON(w, r, status, models.ErrorResponse{Error: models.ErrorBody{
		Code:      code,
		Message:   message,
		RequestID: RequestID(r.Context()),
		Details:   details,
	}})
}
