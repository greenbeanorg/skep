package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Jail templates (ADR 0010 decisions 7 and 14). Building or deleting a
// template is storage:write, which only admin holds: it makes the root
// agent fetch and install packages. Every write is audited in the access
// log with the template as the resource.

// TemplateBackend is what the template handlers need from the service
// layer: in skep-api, the agent client.
type TemplateBackend interface {
	TemplateSources(ctx context.Context) (models.TemplateSources, error)
	Templates(ctx context.Context, opts models.TemplateListOptions) (models.TemplateList, error)
	Template(ctx context.Context, id string) (models.Template, error)
	CreateTemplate(ctx context.Context, req models.TemplateCreate) (models.TemplateCreated, error)
	DeleteTemplate(ctx context.Context, id string) (models.Task, error)
}

func (h *handler) templateRoutes() []route {
	return []route{
		{http.MethodGet, "/storage/template-sources", auth.PermStorageRead, nil, h.templateSources},
		{http.MethodGet, "/storage/templates", auth.PermStorageRead, nil, h.listTemplates},
		{http.MethodPost, "/storage/templates", auth.PermStorageWrite, nil, h.createTemplate},
		{http.MethodGet, "/storage/templates/{id}", auth.PermStorageRead, nil, h.getTemplate},
		{http.MethodDelete, "/storage/templates/{id}", auth.PermStorageWrite, nil, h.deleteTemplate},
	}
}

func templateID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !nodeIDRE.MatchString(id) {
		httpkit.WriteError(w, r, http.StatusBadRequest, httpkit.CodeInvalidRequest, "invalid template ID")
		return "", false
	}
	return id, true
}

// auditTemplate records a template write in the access log.
func (h *handler) auditTemplate(r *http.Request, action, id string, task *models.Task, err error) {
	who, _ := identityFrom(r.Context())
	entry := audit.AccessEntry{
		ActorID: who.User.ID, ActorName: who.User.Username, Action: action,
		ResourceType: models.TemplateResource, ResourceID: id, Outcome: audit.Success,
	}
	if err == nil {
		if task != nil {
			entry.Details = map[string]any{"task_id": task.ID}
		}
	} else {
		entry.Outcome = audit.Failure
		var me *models.Error
		switch {
		case errors.As(err, &me):
			entry.Details = map[string]any{"error_code": me.Code}
			if me.Code != models.CodeInvalidRequest && me.Code != models.CodeAgentUnavailable &&
				me.Code != models.CodeQueueFull && me.Code != models.CodeShuttingDown && me.Code != models.CodeNodeDraining {
				entry.Outcome = audit.Denied
			}
		case errors.Is(err, models.ErrNotFound):
			entry.Details = map[string]any{"error_code": httpkit.CodeNotFound}
		}
	}
	h.writeAudit(r, entry)
}

func (h *handler) templateSources(w http.ResponseWriter, r *http.Request) {
	src, err := h.templates.TemplateSources(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, src)
}

func (h *handler) listTemplates(w http.ResponseWriter, r *http.Request) {
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
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, list)
}

func (h *handler) getTemplate(w http.ResponseWriter, r *http.Request) {
	id, ok := templateID(w, r)
	if !ok {
		return
	}
	t, err := h.templates.Template(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusOK, t)
}

func (h *handler) createTemplate(w http.ResponseWriter, r *http.Request) {
	var req models.TemplateCreate
	if !decodeBody(w, r, &req) {
		return
	}
	var ok bool
	if r, ok = withIdempotencyKey(w, r); !ok {
		return
	}
	created, err := h.templates.CreateTemplate(r.Context(), req)
	h.auditTemplate(r, models.TaskTemplateCreate, created.Template.ID, &created.Task, err)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", Prefix+"/storage/templates/"+created.Template.ID)
	httpkit.WriteJSON(w, r, http.StatusAccepted, created)
}

func (h *handler) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	id, ok := templateID(w, r)
	if !ok {
		return
	}
	if r, ok = withIdempotencyKey(w, r); !ok {
		return
	}
	task, err := h.templates.DeleteTemplate(r.Context(), id)
	h.auditTemplate(r, models.TaskTemplateDelete, id, &task, err)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpkit.WriteJSON(w, r, http.StatusAccepted, task)
}

// noTemplates answers every template operation as a host without
// templates would. It stands in when the handler has no template backend.
type noTemplates struct{}

func (noTemplates) TemplateSources(context.Context) (models.TemplateSources, error) {
	return models.TemplateSources{Packages: []string{},
		Pkgbase: models.TemplateSourceState{Reason: "templates are not available"},
		Dist:    models.TemplateSourceState{Reason: "templates are not available"}}, nil
}
func (noTemplates) Templates(context.Context, models.TemplateListOptions) (models.TemplateList, error) {
	return models.TemplateList{Items: []models.Template{}}, nil
}
func (noTemplates) Template(context.Context, string) (models.Template, error) {
	return models.Template{}, models.ErrNotFound
}
func (noTemplates) CreateTemplate(context.Context, models.TemplateCreate) (models.TemplateCreated, error) {
	return models.TemplateCreated{}, &models.Error{Code: models.CodeStorageFeatureUnsupported, Message: "templates are not available"}
}
func (noTemplates) DeleteTemplate(context.Context, string) (models.Task, error) {
	return models.Task{}, models.ErrNotFound
}
