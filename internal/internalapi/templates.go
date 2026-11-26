package internalapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/internal/templates"
	"github.com/greenbeanorg/skep/pkg/models"
)

// TemplateBackend is what the template handlers need from the agent's
// template service (ADR 0010 decision 7).
type TemplateBackend interface {
	Sources(ctx context.Context) models.TemplateSources
	Templates(ctx context.Context, opts models.TemplateListOptions) (models.TemplateList, error)
	Template(ctx context.Context, id string) (models.Template, error)
	CreateTemplate(ctx context.Context, req models.TemplateCreate, sub templates.Submission) (models.TemplateCreated, error)
	DeleteTemplate(ctx context.Context, id string, sub templates.Submission) (models.Task, error)
}

func (h *handler) registerTemplates(add func(string, http.HandlerFunc)) {
	add("/storage/template-sources", h.templateSources)
	add("/storage/templates", h.templateCollection)
	add("/storage/templates/{id}", h.templateItem)
}

// templateSubmission is the submission of a template write, from the
// headers the middleware already checked.
func templateSubmission(r *http.Request) templates.Submission {
	s := submission(r)
	return templates.Submission{Actor: s.Actor, PeerUID: s.PeerUID, RequestID: s.RequestID,
		SourceIP: s.SourceIP, IdempotencyKey: s.IdempotencyKey}
}

func (h *handler) templateSources(w http.ResponseWriter, r *http.Request) {
	httpkit.WriteJSON(w, r, http.StatusOK, h.templates.Sources(r.Context()))
}

func (h *handler) templateCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req models.TemplateCreate
		if !decode(w, r, &req) {
			return
		}
		created, err := h.templates.CreateTemplate(r.Context(), req, templateSubmission(r))
		if err != nil {
			httpkit.Fail(h.log, w, r, err)
			return
		}
		httpkit.WriteJSON(w, r, http.StatusAccepted, created)
		return
	}
	q := r.URL.Query()
	opts := models.TemplateListOptions{Cursor: q.Get("cursor")}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "limit must be a positive integer")
			return
		}
		opts.Limit = n
	}
	list, err := h.templates.Templates(r.Context(), opts)
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, list)
}

func (h *handler) templateItem(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !idRE.MatchString(id) {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "invalid template ID")
		return
	}
	if r.Method == http.MethodDelete {
		task, err := h.templates.DeleteTemplate(r.Context(), id, templateSubmission(r))
		if err != nil {
			httpkit.Fail(h.log, w, r, err)
			return
		}
		httpkit.WriteJSON(w, r, http.StatusAccepted, task)
		return
	}
	t, err := h.templates.Template(r.Context(), id)
	if err != nil {
		httpkit.Fail(h.log, w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, t)
}
