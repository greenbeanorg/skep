package api

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

// writeJSON writes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.ErrorContext(r.Context(), "api: encode response", "error", err, "request_id", requestID(r.Context()))
		status = http.StatusInternalServerError
		body = []byte(`{"error":{"code":"` + CodeInternal + `","message":"failed to encode the response"}}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(append(body, '\n'))
	}
}

// writeError writes the standard error envelope.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, r, status, models.ErrorResponse{Error: models.ErrorBody{
		Code:      code,
		Message:   message,
		RequestID: requestID(r.Context()),
	}})
}
