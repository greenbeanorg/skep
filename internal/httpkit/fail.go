package httpkit

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/greenbeanorg/skep/pkg/models"
)

// StatusForCode maps the stable error codes of models.Error to HTTP
// statuses. A code that is not listed is treated as an internal error.
var StatusForCode = map[string]int{
	models.CodeInvalidRequest:       http.StatusBadRequest,
	models.CodeResourceBusy:         http.StatusConflict,
	models.CodeIdempotencyConflict:  http.StatusConflict,
	models.CodeNotCancellable:       http.StatusConflict,
	models.CodeQueueFull:            http.StatusServiceUnavailable,
	models.CodeShuttingDown:         http.StatusServiceUnavailable,
	models.CodeNodeDraining:         http.StatusServiceUnavailable,
	models.CodeDrainActive:          http.StatusConflict,
	models.CodeAgentUnavailable:     http.StatusServiceUnavailable,
	models.CodeAgentVersionMismatch: http.StatusServiceUnavailable,
	// Forwarding between nodes (step 8e-1, D37).
	models.CodeNodeUnreachable:     http.StatusServiceUnavailable,
	models.CodeNodeBehind:          http.StatusServiceUnavailable,
	models.CodeNodeClockSkew:       http.StatusServiceUnavailable,
	models.CodeNodeVersionMismatch: http.StatusServiceUnavailable,

	// VM and storage operations (ADR 0009). A request that is well formed
	// but names something this host does not offer is 422; one that the
	// current state of the VM or the host refuses is 409.
	models.CodeEngineUnsupported:   http.StatusUnprocessableEntity,
	models.CodeFirmwareUnsupported: http.StatusUnprocessableEntity,
	models.CodePoolNotAllowed:      http.StatusUnprocessableEntity,
	models.CodeBridgeNotAllowed:    http.StatusUnprocessableEntity,
	models.CodeMediaNotFound:       http.StatusUnprocessableEntity,
	models.CodeNotImplemented:      http.StatusNotImplemented,
	models.CodeInsufficientSpace:   http.StatusConflict,
	models.CodeInsufficientMemory:  http.StatusConflict,
	models.CodeBhyveUnavailable:    http.StatusConflict,
	models.CodeStorageUnavailable:  http.StatusConflict,
	models.CodeVMNameTaken:         http.StatusConflict,
	models.CodeVMNotStopped:        http.StatusConflict,
	models.CodeVMMACInUse:          http.StatusConflict,
	models.CodeVMNotRunning:        http.StatusConflict,
	models.CodeVMNotProvisioned:    http.StatusConflict,
	models.CodeStopTimeout:         http.StatusConflict,
	models.CodeVMStartFailed:       http.StatusConflict,
	models.CodeConsoleBusy:         http.StatusConflict,

	// Storage management (ADR 0010).
	models.CodeSnapshotLimit:             http.StatusConflict,
	models.CodeDiskHasSnapshots:          http.StatusConflict,
	models.CodeDiskDeviceMissing:         http.StatusConflict,
	models.CodeSnapshotNameTaken:         http.StatusConflict,
	models.CodeSnapshotNotProvisioned:    http.StatusConflict,
	models.CodeSnapshotHasClones:         http.StatusConflict,
	models.CodeSnapshotNotLatest:         http.StatusConflict,
	models.CodeSnapshotIncomplete:        http.StatusConflict,
	models.CodeStorageLocked:             http.StatusConflict,
	models.CodeStorageFeatureUnsupported: http.StatusUnprocessableEntity,

	// Jail templates (ADR 0010 decision 7, step 6). Most of these end a
	// build task; the statuses apply when a request is refused with one.
	models.CodeReleaseUnsupported:      http.StatusUnprocessableEntity,
	models.CodeJailParentInsecure:      http.StatusConflict,
	models.CodeTemplateScriptUnknown:   http.StatusUnprocessableEntity,
	models.CodeTemplateTriggerLeft:     http.StatusInternalServerError,
	models.CodeTemplateHostChanged:     http.StatusInternalServerError,
	models.CodeTemplateBuildFailed:     http.StatusInternalServerError,
	models.CodeTemplateNameTaken:       http.StatusConflict,
	models.CodeTemplateNotProvisioned:  http.StatusConflict,
	models.CodeTemplateArchiveMismatch: http.StatusUnprocessableEntity,

	// Jails (ADR 0010 decisions 8, 9, 12, and 13).
	models.CodeContainerNameTaken:       http.StatusConflict,
	models.CodeContainerNotStopped:      http.StatusConflict,
	models.CodeContainerNotRunning:      http.StatusConflict,
	models.CodeContainerNotProvisioned:  http.StatusConflict,
	models.CodeContainerStarted:         http.StatusConflict,
	models.CodeContainerStartFailed:     http.StatusConflict,
	models.CodeContainerSSHUnavailable:  http.StatusUnprocessableEntity,
	models.CodeMlockRequiresMemoryLimit: http.StatusUnprocessableEntity,
	models.CodeNameserversRequireNIC:    http.StatusUnprocessableEntity,
	models.CodeEpairUnavailable:         http.StatusConflict,
	// Guest-port filters (ADR 0011), on jail and VM starts.
	models.CodePortFilterUnavailable:   http.StatusConflict,
	models.CodePortFilterFailed:        http.StatusConflict,
	models.CodeRctlUnavailable:         http.StatusConflict,
	models.CodeDevfsRulesetConflict:    http.StatusConflict,
	models.CodeCloneAddressingRequired: http.StatusUnprocessableEntity,
	models.CodeContainerAddressInUse:   http.StatusConflict,
	models.CodeContainerMACInvalid:     http.StatusUnprocessableEntity,
	models.CodeContainerMACInUse:       http.StatusConflict,
	models.CodeContainerQuarantined:    http.StatusConflict,
	models.CodeContainerRemoveFailed:   http.StatusConflict,

	models.CodeContainerLockedMemoryAboveMax:  http.StatusUnprocessableEntity,
	models.CodeContainerLockedMemoryExhausted: http.StatusConflict,
	models.CodeContainerDiskQuotaBelowUsage:   http.StatusUnprocessableEntity,
}

// Fail maps a service error to an error response. Internal error text is
// logged and never sent to the client.
func Fail(log *slog.Logger, w http.ResponseWriter, r *http.Request, err error) {
	var me *models.Error
	if errors.As(err, &me) {
		if status, ok := StatusForCode[me.Code]; ok {
			WriteErrorDetails(w, r, status, me.Code, me.Message, me.Details)
			return
		}
	}
	switch {
	case errors.Is(err, models.ErrNotFound):
		WriteError(w, r, http.StatusNotFound, CodeNotFound, "the requested resource does not exist")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		WriteError(w, r, http.StatusServiceUnavailable, CodeUnavailable, "the request was cancelled or timed out")
	default:
		log.ErrorContext(r.Context(), "http: request failed",
			"error", err, "path", r.URL.Path, "request_id", RequestID(r.Context()))
		WriteError(w, r, http.StatusInternalServerError, CodeInternal, "internal server error")
	}
}

// ReadOnly allows GET and HEAD and answers anything else with a JSON 405.
// Routing by path and checking the method here, instead of using method
// patterns in the mux, keeps 405 responses in the standard error envelope.
func ReadOnly(next http.HandlerFunc) http.Handler {
	return Methods(next, http.MethodGet, http.MethodHead)
}

// Methods allows the listed methods and answers anything else with a JSON
// 405 that names them.
func Methods(next http.HandlerFunc, methods ...string) http.Handler {
	allow := ""
	for i, m := range methods {
		if i > 0 {
			allow += ", "
		}
		allow += m
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, m := range methods {
			if r.Method == m {
				next(w, r)
				return
			}
		}
		w.Header().Set("Allow", allow)
		WriteError(w, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
			"method "+r.Method+" is not allowed on this endpoint")
	})
}

// NotFound answers any request with the 404 error envelope.
func NotFound(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusNotFound, CodeNotFound, "no such endpoint: "+r.URL.Path)
}

// IDPattern is the accepted shape of a resource ID in a URL.
const IDPattern = `^[A-Za-z0-9._-]{1,64}$`
