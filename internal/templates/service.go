package templates

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/ids"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Page sizes of the template list.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// Task limits. The build's 30 minutes are the ADR's (decision 7b).
const (
	createTimeout = 30 * time.Minute
	deleteTimeout = 5 * time.Minute
)

// nameRE is the shape of a template name, as the schema checks it.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// Engine is what the service needs from the task engine.
type Engine interface {
	Submit(ctx context.Context, req tasks.Request) (tasks.Task, bool, error)
	Refuse(ctx context.Context, req tasks.Request, outcome audit.Outcome, cause *models.Error)
	LookupKey(ctx context.Context, key string) (tasks.Task, bool, error)
}

// Options configures a Service.
type Options struct {
	NodeID string
	Store  *database.TemplateStore
	Engine Engine
	// Host gives the confinement and the template operations.
	Host hostops.Ops
	// Packages are storage.template_packages.
	Packages []string
	Logger   *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
}

// Service implements the template operations of the internal API.
type Service struct {
	nodeID   string
	store    *database.TemplateStore
	engine   Engine
	conf     hostops.Confinement
	host     hostops.Templates
	packages []string
	builder  *Builder
	log      *slog.Logger
	now      func() time.Time
}

// New returns a Service. Register its Definitions before the engine
// starts.
func New(opts Options) (*Service, error) {
	if opts.NodeID == "" || opts.Store == nil || opts.Engine == nil || opts.Host.Templates == nil {
		return nil, errors.New("templates: a node ID, the store, a task engine, and the template host operations are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{
		nodeID: opts.NodeID, store: opts.Store, engine: opts.Engine, conf: opts.Host.Conf,
		host: opts.Host.Templates, packages: append([]string(nil), opts.Packages...),
		builder: &Builder{Host: opts.Host.Templates, Log: opts.Logger}, log: opts.Logger, now: opts.Now,
	}, nil
}

// Definitions are the template task types.
func (s *Service) Definitions() []tasks.Definition {
	return []tasks.Definition{
		{Type: models.TaskTemplateCreate, Cancellable: true, Timeout: createTimeout, Run: s.runCreate},
		{Type: models.TaskTemplateDelete, Timeout: deleteTimeout, Run: s.runDelete},
	}
}

// Submission is who a write is made for and how it arrived: the actor as
// skep-api asserts it, the peer as the kernel verified it.
type Submission struct {
	Actor          models.TaskActor
	PeerUID        uint32
	RequestID      string
	SourceIP       string
	IdempotencyKey string
}

func (sub Submission) request(taskType, templateID string, payload []byte) tasks.Request {
	req := tasks.Request{
		Type: taskType, Payload: payload, IdempotencyKey: sub.IdempotencyKey,
		Actor: sub.Actor, PeerUID: sub.PeerUID, RequestID: sub.RequestID, SourceIP: sub.SourceIP,
	}
	if templateID != "" {
		req.ResourceType, req.ResourceID = models.TemplateResource, templateID
	}
	return req
}

// refuse records a submission refused before the engine saw it.
func (s *Service) refuse(ctx context.Context, req tasks.Request, err error) error {
	var me *models.Error
	if !errors.As(err, &me) {
		return err
	}
	outcome := audit.Denied
	if me.Code == models.CodeInvalidRequest {
		outcome = audit.Failure
	}
	req.Payload, req.Companion = nil, nil
	s.engine.Refuse(ctx, req, outcome, me)
	return err
}

// Sources reports what this host can build templates from.
func (s *Service) Sources(ctx context.Context) models.TemplateSources {
	out := models.TemplateSources{Packages: append([]string{}, s.packages...)}
	rel, err := s.host.HostRelease(ctx)
	if err != nil {
		s.log.Warn("templates: cannot read the host's release", "error", err)
		out.Pkgbase.Reason = "the host's release cannot be read"
		out.Dist.Reason = out.Pkgbase.Reason
		return out
	}
	out.HostRelease = rel
	out.Pkgbase = s.pkgbaseState(rel)
	if ok, why := s.host.DistAvailable(); ok {
		out.Dist.Available = true
	} else {
		out.Dist.Reason = why
	}
	return out
}

func (s *Service) pkgbaseState(hostRelease string) models.TemplateSourceState {
	r, err := ParseRelease(hostRelease)
	switch {
	case err != nil:
		return models.TemplateSourceState{Reason: "the host's release cannot be read"}
	case len(s.packages) == 0:
		return models.TemplateSourceState{Reason: "storage.template_packages is empty"}
	}
	if _, ok := AllowlistFor(r.MajorMinor()); !ok {
		return models.TemplateSourceState{Reason: "Skep has no package-script allowlist for FreeBSD " + r.MajorMinor()}
	}
	if ok, why := s.host.PkgbaseAvailable(); !ok {
		return models.TemplateSourceState{Reason: why}
	}
	return models.TemplateSourceState{Available: true}
}

// decorate adds what a template is compared against now.
func (s *Service) decorate(ctx context.Context, list []models.Template) {
	rel, err := s.host.HostRelease(ctx)
	if err != nil {
		return
	}
	for i := range list {
		list[i].HostRelease = rel
		list[i].PatchDiffers = list[i].Release != "" && PatchDiffers(list[i].Release, rel)
	}
}

// Templates lists the templates by name.
func (s *Service) Templates(ctx context.Context, opts models.TemplateListOptions) (models.TemplateList, error) {
	limit := opts.Limit
	switch {
	case limit <= 0:
		limit = DefaultPageSize
	case limit > MaxPageSize:
		limit = MaxPageSize
	}
	list, next, err := s.store.List(ctx, limit, opts.Cursor)
	if err != nil {
		return models.TemplateList{}, err
	}
	s.decorate(ctx, list)
	return models.TemplateList{Items: list, NextCursor: next}, nil
}

// Template returns one template.
func (s *Service) Template(ctx context.Context, id string) (models.Template, error) {
	if !hostops.ValidID(id) {
		return models.Template{}, fmt.Errorf("template %q: %w", id, models.ErrNotFound)
	}
	t, err := s.store.Get(ctx, id)
	if err != nil {
		return models.Template{}, err
	}
	list := []models.Template{t}
	s.decorate(ctx, list)
	return list[0], nil
}

// createPayload is the payload of container.template.create. Request is
// the normalised request, so a replay with the same idempotency key is
// recognised although the generated ID differs.
type createPayload struct {
	Request    json.RawMessage `json:"request"`
	TemplateID string          `json:"template_id"`
	// Packages are the configured packages at the time of the request,
	// so the task builds what the request was checked against.
	Packages []string `json:"packages,omitempty"`
}

func canonical(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return tasks.CanonicalPayload(raw)
}

func (s *Service) validCreate(ctx context.Context, req models.TemplateCreate) error {
	if !nameRE.MatchString(req.Name) {
		return models.Invalid("a template name is 1 to 63 letters, digits, '.', '_', or '-', starting with a letter or digit")
	}
	kind, err := s.conf.PoolKind(req.Pool)
	if err != nil {
		return err
	}
	if kind != hostops.PoolKindZFS {
		// Templates in a directory pool are not built in this step; see
		// the ADR's step 6 implementation notes.
		return &models.Error{Code: models.CodeStorageFeatureUnsupported,
			Message: "templates can be built in ZFS pools only; " + req.Pool + " is a directory pool",
			Details: map[string]any{"pool": req.Pool, "pool_type": kind}}
	}
	src := s.Sources(ctx)
	switch req.Source {
	case models.TemplateSourcePkgbase:
		if !src.Pkgbase.Available {
			if src.HostRelease != "" {
				if r, err := ParseRelease(src.HostRelease); err == nil {
					if _, ok := AllowlistFor(r.MajorMinor()); !ok {
						return releaseUnsupported(src.Pkgbase.Reason, "", src.HostRelease)
					}
				}
			}
			return sourceUnavailable(req.Source, src.Pkgbase.Reason)
		}
	case models.TemplateSourceDist:
		if !src.Dist.Available {
			return sourceUnavailable(req.Source, src.Dist.Reason)
		}
	default:
		return models.Invalid(`the source is "pkgbase" or "dist"`)
	}
	return nil
}

// CreateTemplate validates a request, reserves the template's row, and
// submits the task that builds it.
func (s *Service) CreateTemplate(ctx context.Context, req models.TemplateCreate, sub Submission) (models.TemplateCreated, error) {
	refused := sub.request(models.TaskTemplateCreate, "", nil)
	if err := s.validCreate(ctx, req); err != nil {
		return models.TemplateCreated{}, s.refuse(ctx, refused, err)
	}
	request, err := canonical(req)
	if err != nil {
		return models.TemplateCreated{}, err
	}
	if existing, found, err := s.replay(ctx, sub, request); err != nil {
		return models.TemplateCreated{}, s.refuse(ctx, refused, err)
	} else if found {
		var p createPayload
		_ = json.Unmarshal(existing.Payload, &p)
		t, err := s.store.Get(ctx, p.TemplateID)
		if errors.Is(err, models.ErrNotFound) {
			t = models.Template{ID: p.TemplateID, Name: req.Name, Pool: req.Pool, Source: req.Source, Packages: []string{}}
		} else if err != nil {
			return models.TemplateCreated{}, err
		}
		return models.TemplateCreated{Template: t, Task: existing.Public()}, nil
	}
	t := models.Template{Name: req.Name, Pool: req.Pool, Source: req.Source, Packages: []string{}, CreatedAt: s.now().UTC()}
	if req.Source == models.TemplateSourcePkgbase {
		t.Packages = append(t.Packages, s.packages...)
	}
	if t.ID, err = ids.NewUUID(); err != nil {
		return models.TemplateCreated{}, err
	}
	payload, err := canonical(createPayload{Request: request, TemplateID: t.ID, Packages: t.Packages})
	if err != nil {
		return models.TemplateCreated{}, err
	}
	submit := sub.request(models.TaskTemplateCreate, t.ID, payload)
	submit.Companion = func(ctx context.Context, tx *sql.Tx) error {
		return s.store.InsertTx(ctx, tx, s.nodeID, t)
	}
	// A refusal by the companion (template_name_taken) is audited by the
	// engine.
	task, _, err := s.engine.Submit(ctx, submit)
	if err != nil {
		return models.TemplateCreated{}, err
	}
	stored, err := s.store.Get(ctx, t.ID)
	if err != nil {
		return models.TemplateCreated{}, err
	}
	return models.TemplateCreated{Template: stored, Task: task.Public()}, nil
}

// replay finds the task an earlier request with the same idempotency key
// created. A key used for another request is idempotency_key_conflict.
func (s *Service) replay(ctx context.Context, sub Submission, request json.RawMessage) (tasks.Task, bool, error) {
	if sub.IdempotencyKey == "" {
		return tasks.Task{}, false, nil
	}
	existing, found, err := s.engine.LookupKey(ctx, sub.IdempotencyKey)
	if err != nil || !found {
		return tasks.Task{}, false, err
	}
	var p createPayload
	same := existing.Type == models.TaskTemplateCreate && existing.Actor.ID == sub.Actor.ID &&
		json.Unmarshal(existing.Payload, &p) == nil
	if same {
		earlier, err := tasks.CanonicalPayload(p.Request)
		same = err == nil && bytes.Equal(earlier, request)
	}
	if !same {
		return tasks.Task{}, false, &models.Error{Code: models.CodeIdempotencyConflict,
			Message: "this idempotency key was already used for a different request"}
	}
	return existing, true, nil
}

// CreateResult is the result of container.template.create.
type CreateResult struct {
	TemplateID string `json:"template_id"`
	Name       string `json:"name"`
	BuildResult
}

// AuditDetails implements tasks.Audited.
func (r CreateResult) AuditDetails() map[string]any {
	return map[string]any{"template_id": r.TemplateID, "template_name": r.Name, "release": r.Release,
		"host_release": r.HostRelease, "snapshot": r.Snapshot, "archive_sha256": r.ArchiveSHA256,
		"commands_run": r.Commands, "host_files_compared": r.HostFiles}
}

// failure turns an error the user can act on into a task failure with its
// code. Anything else stays an internal error, logged.
func failure(err error) error {
	var me *models.Error
	if errors.As(err, &me) {
		return &tasks.Failure{Code: me.Code, Message: me.Message, Details: me.Details}
	}
	return err
}

func (s *Service) runCreate(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	var p createPayload
	if err := json.Unmarshal(task.Payload, &p); err != nil || !hostops.ValidID(p.TemplateID) || p.TemplateID != task.ResourceID {
		return nil, &tasks.Failure{Code: models.CodeInvalidRequest, Message: "the task payload is not a template request"}
	}
	t, err := s.store.Get(ctx, p.TemplateID)
	if err != nil {
		return nil, err
	}
	if t.Provisioned {
		return nil, &tasks.Failure{Code: models.CodeInvalidRequest, Message: "the template is already built"}
	}
	spec := BuildSpec{Pool: t.Pool, TemplateID: t.ID, Source: t.Source, Packages: p.Packages}
	res, err := s.builder.Build(ctx, spec, func(msg string) { report.Progress(msg, nil) })
	if err != nil {
		var me *models.Error
		if errors.As(err, &me) && me.Code == models.CodeTemplateHostChanged {
			report.Progress("the host files changed during the build: "+me.Message, me.Details)
		}
		return nil, failure(err)
	}
	// The row is written on its own context: a template that exists on
	// the host must be recorded even if the task was cancelled just now.
	rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.store.SetProvisioned(rctx, t.ID, res.Release, res.ArchiveSHA256, res.UsedBytes, s.now().UTC()); err != nil {
		return nil, err
	}
	return CreateResult{TemplateID: t.ID, Name: t.Name, BuildResult: res}, nil
}

// DeleteTemplate submits the task that destroys a template. A template
// still in use by a jail cannot be deleted (the schema refuses it; jails
// arrive in step 7).
func (s *Service) DeleteTemplate(ctx context.Context, id string, sub Submission) (models.Task, error) {
	refused := sub.request(models.TaskTemplateDelete, id, nil)
	if !hostops.ValidID(id) {
		return models.Task{}, fmt.Errorf("template %q: %w", id, models.ErrNotFound)
	}
	if _, err := s.store.Get(ctx, id); err != nil {
		return models.Task{}, s.refuse(ctx, refused, err)
	}
	payload, err := canonical(struct {
		TemplateID string `json:"template_id"`
	}{id})
	if err != nil {
		return models.Task{}, err
	}
	task, _, err := s.engine.Submit(ctx, sub.request(models.TaskTemplateDelete, id, payload))
	if err != nil {
		return models.Task{}, err
	}
	return task.Public(), nil
}

// DeleteResult is the result of container.template.delete.
type DeleteResult struct {
	TemplateID string        `json:"template_id"`
	Destroyed  []DatasetInfo `json:"datasets_destroyed"`
}

// DatasetInfo names a dataset a task destroyed.
type DatasetInfo struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	UsedBytes int64  `json:"used_bytes"`
}

// AuditDetails implements tasks.Audited.
func (r DeleteResult) AuditDetails() map[string]any {
	return map[string]any{"template_id": r.TemplateID, "datasets_destroyed": r.Destroyed}
}

func (s *Service) runDelete(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	t, err := s.store.Get(ctx, task.ResourceID)
	if err != nil {
		return nil, err
	}
	// A build jail of this template, left by a crash before the start-up
	// sweep could see it, goes first.
	if err := s.host.RemoveBuildJail(ctx, t.ID); err != nil {
		return nil, failure(err)
	}
	report.Progress("destroying the template dataset", nil)
	destroyed, err := s.host.DestroyTemplate(ctx, t.Pool, t.ID)
	if err != nil {
		return nil, failure(err)
	}
	if err := s.store.Delete(ctx, t.ID); err != nil {
		return nil, err
	}
	res := DeleteResult{TemplateID: t.ID, Destroyed: []DatasetInfo{}}
	for _, d := range destroyed {
		res.Destroyed = append(res.Destroyed, DatasetInfo{Name: d.Name, Type: d.Type, UsedBytes: d.UsedBytes})
	}
	return res, nil
}

// SweepBuildJails removes every skep-build-* jail left on the host, which
// only a crash or a kill of the agent during a build can leave (the step 6
// decision, item 2). The agent calls it at start, before it serves its
// socket. Each removal is logged as a warning event, and every completed
// sweep logs one line with its counts, so a sweep that found nothing is
// visible too.
func (s *Service) SweepBuildJails(ctx context.Context) error {
	ids, err := s.host.BuildJails(ctx)
	if err != nil {
		return fmt.Errorf("templates: list the build jails: %w", err)
	}
	var errs []error
	removed := 0
	defer func() {
		s.log.Info("templates: build jail sweep finished", "event", "build_jail_sweep",
			"found", len(ids), "removed", removed, "failed", len(errs))
	}()
	for _, id := range ids {
		name, _ := hostops.BuildJailName(id)
		if err := s.host.RemoveBuildJail(ctx, id); err != nil {
			s.log.Error("templates: a leftover build jail could not be removed", "event", "build_jail_sweep_failed",
				"jail", name, "template_id", id, "error", err)
			errs = append(errs, err)
			continue
		}
		removed++
		s.log.Warn("templates: removed a build jail left by an earlier run", "event", "build_jail_removed",
			"jail", name, "template_id", id)
	}
	return errors.Join(errs...)
}
