package containers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/ids"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
)

// taskSlack is added to the jail timeouts to bound a task.
const taskSlack = time.Minute

// Definitions are the container task types. None can be cancelled while
// it runs: a half-created or half-started jail is the user's to look at,
// not something a cancel should leave behind.
func (s *Service) Definitions() []tasks.Definition {
	maxStop := models.MaxContainerStopTimeoutSeconds * time.Second
	return []tasks.Definition{
		{Type: models.TaskContainerCreate, Timeout: 5 * time.Minute, Run: s.runCreate},
		{Type: models.TaskContainerUpdate, Timeout: 2 * time.Minute, Run: s.runUpdate},
		{Type: models.TaskContainerDelete, Timeout: s.startTimeout + maxStop + 5*time.Minute, Run: s.runDelete},
		{Type: models.TaskContainerStart, Timeout: s.startTimeout + maxStop + taskSlack, Run: s.runStart},
		{Type: models.TaskContainerStop, Timeout: s.startTimeout + maxStop + taskSlack, Run: s.runStop},
		{Type: models.TaskContainerRestart, Timeout: 2*s.startTimeout + 2*maxStop + taskSlack, Run: s.runRestart},
	}
}

func canonical(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return tasks.CanonicalPayload(raw)
}

// optionsError refuses the opt-ins this host cannot give. mlock needs an
// RCTL memory limit (decision 9), and limits arrive in step 9, so it is
// always refused for now.
func optionsError(o models.ContainerOptions) error {
	if o.Mlock {
		return &models.Error{Code: models.CodeMlockRequiresMemoryLimit,
			Message: "mlock is accepted only together with an RCTL memory limit, because locked memory cannot be paged out; jail memory limits are not available yet"}
	}
	return nil
}

// checkSSH validates an SSH key and refuses it for a template without
// sshd (amendment g). It returns the normalised key.
func (s *Service) checkSSH(ctx context.Context, t models.Template, key string) (string, error) {
	if key == "" {
		return "", nil
	}
	key, err := hostops.NormalizeSSHPublicKey(key)
	if err != nil {
		return "", err
	}
	has, err := s.jails.TemplateHasSSHD(ctx, t.Pool, t.ID)
	if err != nil {
		return "", err
	}
	if !has {
		return "", &models.Error{Code: models.CodeContainerSSHUnavailable,
			Message: "the template " + t.Name + " has no sshd, so an SSH key cannot be used; build a template with FreeBSD-ssh in storage.template_packages",
			Details: map[string]any{"template_id": t.ID, "template_name": t.Name}}
	}
	return key, nil
}

// normalizeCreate validates a create request and returns the container it
// describes, without an ID.
func (s *Service) normalizeCreate(ctx context.Context, req models.ContainerCreate) (models.Container, models.Template, error) {
	if !nameRE.MatchString(req.Name) {
		return models.Container{}, models.Template{}, models.Invalid("a container name is 1 to 63 letters, digits, '.', '_', or '-', starting with a letter or digit")
	}
	c := models.Container{Name: req.Name, Hostname: req.Hostname, TemplateID: req.TemplateID}
	if c.Hostname == "" {
		c.Hostname = req.Name
	}
	if !hostops.ValidHostname(c.Hostname) {
		return c, models.Template{}, models.Invalid("the hostname must be a DNS name: labels of letters, digits, and hyphens, separated by dots" +
			" (a name with '_' needs an explicit hostname)")
	}
	if req.Options != nil {
		c.Options = *req.Options
	}
	if err := optionsError(c.Options); err != nil {
		return c, models.Template{}, err
	}
	if !hostops.ValidID(req.TemplateID) {
		return c, models.Template{}, models.Invalid("template_id must be a template's ID")
	}
	t, err := s.templates.Get(ctx, req.TemplateID)
	if errors.Is(err, models.ErrNotFound) {
		return c, t, models.Invalid("there is no template with this ID")
	}
	if err != nil {
		return c, t, err
	}
	if !t.Provisioned {
		return c, t, &models.Error{Code: models.CodeTemplateNotProvisioned,
			Message: "the template was never built; it can only be deleted", Details: map[string]any{"template_id": t.ID}}
	}
	if err := s.checkRelease(ctx, t); err != nil {
		return c, t, err
	}
	if c.SSHAuthorizedKey, err = s.checkSSH(ctx, t, req.SSHAuthorizedKey); err != nil {
		return c, t, err
	}
	c.TemplateName, c.Pool, c.Release = t.Name, t.Pool, t.Release
	return c, t, nil
}

// createPayload is the payload of container.create. Request is the
// normalised request, so a replay with the same idempotency key is
// recognised although the generated ID differs.
type createPayload struct {
	Request     json.RawMessage `json:"request"`
	ContainerID string          `json:"container_id"`
}

// CreateContainer validates a request, reserves the container's row, and
// submits the task that clones and configures it.
func (s *Service) CreateContainer(ctx context.Context, req models.ContainerCreate, sub Submission) (models.ContainerCreated, error) {
	refused := sub.request(models.TaskContainerCreate, "", nil)
	c, _, err := s.normalizeCreate(ctx, req)
	if err != nil {
		return models.ContainerCreated{}, s.refuse(ctx, refused, err)
	}
	normal := models.ContainerCreate{Name: c.Name, Hostname: c.Hostname, TemplateID: c.TemplateID,
		Options: &c.Options, SSHAuthorizedKey: c.SSHAuthorizedKey}
	request, err := canonical(normal)
	if err != nil {
		return models.ContainerCreated{}, err
	}
	if existing, found, err := s.replay(ctx, sub, request); err != nil {
		return models.ContainerCreated{}, s.refuse(ctx, refused, err)
	} else if found {
		var p createPayload
		_ = json.Unmarshal(existing.Payload, &p)
		got, err := s.store.Get(ctx, p.ContainerID)
		if errors.Is(err, models.ErrNotFound) {
			got = c
			got.ID = p.ContainerID
		} else if err != nil {
			return models.ContainerCreated{}, err
		}
		return models.ContainerCreated{Container: got, Task: existing.Public()}, nil
	}
	if c.ID, err = ids.NewUUID(); err != nil {
		return models.ContainerCreated{}, err
	}
	c.CreatedAt = s.now().UTC()
	payload, err := canonical(createPayload{Request: request, ContainerID: c.ID})
	if err != nil {
		return models.ContainerCreated{}, err
	}
	submit := sub.request(models.TaskContainerCreate, c.ID, payload)
	submit.Companion = func(ctx context.Context, tx *sql.Tx) error {
		return s.store.InsertTx(ctx, tx, s.nodeID, c)
	}
	task, _, err := s.engine.Submit(ctx, submit)
	if err != nil {
		return models.ContainerCreated{}, err
	}
	stored, err := s.store.Get(ctx, c.ID)
	if err != nil {
		return models.ContainerCreated{}, err
	}
	return models.ContainerCreated{Container: stored, Task: task.Public()}, nil
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
	same := existing.Type == models.TaskContainerCreate && existing.Actor.ID == sub.Actor.ID &&
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

// jailFiles is what Skep writes into a jail before its first start: with
// an SSH key, sshd on and the key for root; without, nothing.
func jailFiles(c models.Container) hostops.JailFiles {
	if c.SSHAuthorizedKey == "" {
		return hostops.JailFiles{}
	}
	return hostops.JailFiles{RCConf: hostops.SSHRCConf, AuthorizedKey: c.SSHAuthorizedKey}
}

// CreateResult is the result of container.create.
type CreateResult struct {
	ContainerID string `json:"container_id"`
	Name        string `json:"name"`
	TemplateID  string `json:"template_id"`
	Dataset     string `json:"dataset"`
	Origin      string `json:"origin"`
	SSHKey      bool   `json:"ssh_key_installed"`
}

// AuditDetails implements tasks.Audited.
func (r CreateResult) AuditDetails() map[string]any {
	return map[string]any{"container_id": r.ContainerID, "container_name": r.Name, "template_id": r.TemplateID,
		"dataset": r.Dataset, "origin": r.Origin, "ssh_key_installed": r.SSHKey}
}

func (s *Service) runCreate(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	var p createPayload
	if err := json.Unmarshal(task.Payload, &p); err != nil || !hostops.ValidID(p.ContainerID) || p.ContainerID != task.ResourceID {
		return nil, &tasks.Failure{Code: models.CodeInvalidRequest, Message: "the task payload is not a container request"}
	}
	c, err := s.store.Get(ctx, p.ContainerID)
	if err != nil {
		return nil, err
	}
	if c.Provisioned {
		return nil, &tasks.Failure{Code: models.CodeInvalidRequest, Message: "the container is already provisioned"}
	}
	report.Progress("checking the jail directory", nil)
	if err := s.jails.EnsureJailsParent(ctx, c.Pool); err != nil {
		return nil, failure(err)
	}
	report.Progress("cloning the template", nil)
	ds, err := s.jails.CreateJailRoot(ctx, c.Pool, c.TemplateID, c.ID)
	if err != nil {
		return nil, failure(err)
	}
	files := jailFiles(c)
	if files.AuthorizedKey != "" {
		report.Progress("writing rc.conf and root's SSH key into the jail", nil)
		if err := s.jails.ConfigureJail(ctx, c.Pool, c.ID, files); err != nil {
			s.destroyAfterFailure(c)
			return nil, failure(err)
		}
	}
	// Recorded on its own context: a jail root that exists must be
	// recorded even if the request went away.
	rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.store.SetProvisioned(rctx, c.ID, s.now().UTC()); err != nil {
		return nil, err
	}
	return CreateResult{ContainerID: c.ID, Name: c.Name, TemplateID: c.TemplateID, Dataset: ds.Name,
		Origin: ds.Origin, SSHKey: files.AuthorizedKey != ""}, nil
}

// destroyAfterFailure removes a jail root a failed create left, on its own
// context. The row stays, unprovisioned, for the user to delete.
func (s *Service) destroyAfterFailure(c models.Container) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := s.jails.DestroyJailRoot(ctx, c.Pool, c.ID); err != nil {
		s.log.Error("containers: the jail root of a failed create could not be destroyed", "container_id", c.ID, "error", err)
	}
}

// isRunning asks the host whether a container's jail runs.
func (s *Service) isRunning(ctx context.Context, id string) (bool, error) {
	running, err := s.jails.RunningJails(ctx)
	if err != nil {
		return false, err
	}
	_, ok := running[id]
	return ok, nil
}

func notStopped(c models.Container) error {
	return &models.Error{Code: models.CodeContainerNotStopped, Message: "the jail is running; stop it first",
		Details: map[string]any{"container_id": c.ID}}
}

func notProvisioned(c models.Container) error {
	return &models.Error{Code: models.CodeContainerNotProvisioned,
		Message: "the container was never provisioned; it can only be deleted", Details: map[string]any{"container_id": c.ID}}
}

func started(c models.Container) error {
	return &models.Error{Code: models.CodeContainerStarted,
		Message: "the jail has started at least once, so Skep no longer writes into its tree (an SSH key change is the jail's own business now)",
		Details: map[string]any{"container_id": c.ID}}
}

// updatePayload is the payload of container.update: the container after
// the change.
type updatePayload struct {
	Hostname         string                  `json:"hostname"`
	Options          models.ContainerOptions `json:"options"`
	SSHAuthorizedKey string                  `json:"ssh_authorized_key"`
	// WriteFiles is true when the SSH key changed, which writes into the
	// jail's tree.
	WriteFiles bool `json:"write_files"`
}

// UpdateContainer validates a change to a stopped container and submits
// the task that applies it.
func (s *Service) UpdateContainer(ctx context.Context, id string, req models.ContainerUpdate, sub Submission) (models.Task, error) {
	if !hostops.ValidID(id) {
		return models.Task{}, fmt.Errorf("container %q: %w", id, models.ErrNotFound)
	}
	refused := sub.request(models.TaskContainerUpdate, id, nil)
	p, err := s.normalizeUpdate(ctx, id, req)
	if err != nil {
		return models.Task{}, s.refuse(ctx, refused, err)
	}
	payload, err := canonical(p)
	if err != nil {
		return models.Task{}, err
	}
	task, _, err := s.engine.Submit(ctx, sub.request(models.TaskContainerUpdate, id, payload))
	if err != nil {
		return models.Task{}, err
	}
	return task.Public(), nil
}

func (s *Service) normalizeUpdate(ctx context.Context, id string, req models.ContainerUpdate) (updatePayload, error) {
	c, err := s.store.Get(ctx, id)
	if err != nil {
		return updatePayload{}, err
	}
	if !c.Provisioned {
		return updatePayload{}, notProvisioned(c)
	}
	if req.Hostname == nil && req.Options == nil && req.SSHAuthorizedKey == nil {
		return updatePayload{}, models.Invalid("nothing to change: give hostname, options, or ssh_authorized_key")
	}
	p := updatePayload{Hostname: c.Hostname, Options: c.Options, SSHAuthorizedKey: c.SSHAuthorizedKey}
	if req.Hostname != nil {
		if !hostops.ValidHostname(*req.Hostname) {
			return p, models.Invalid("the hostname must be a DNS name: labels of letters, digits, and hyphens, separated by dots")
		}
		p.Hostname = *req.Hostname
	}
	if req.Options != nil {
		if err := optionsError(*req.Options); err != nil {
			return p, err
		}
		p.Options = *req.Options
	}
	if req.SSHAuthorizedKey != nil {
		key := *req.SSHAuthorizedKey
		if key != "" {
			t, err := s.templates.Get(ctx, c.TemplateID)
			if err != nil {
				return p, err
			}
			if key, err = s.checkSSH(ctx, t, key); err != nil {
				return p, err
			}
		}
		if key != c.SSHAuthorizedKey {
			if c.StartedOnce {
				return p, started(c)
			}
			p.SSHAuthorizedKey, p.WriteFiles = key, true
		}
	}
	if running, err := s.isRunning(ctx, id); err != nil {
		return p, err
	} else if running {
		return p, notStopped(c)
	}
	return p, nil
}

func (s *Service) runUpdate(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	var p updatePayload
	if err := json.Unmarshal(task.Payload, &p); err != nil {
		return nil, &tasks.Failure{Code: models.CodeInvalidRequest, Message: "the task payload is not a container update"}
	}
	c, err := s.store.Get(ctx, task.ResourceID)
	if err != nil {
		return nil, err
	}
	if running, err := s.isRunning(ctx, c.ID); err != nil {
		return nil, err
	} else if running {
		return nil, failure(notStopped(c))
	}
	c.Hostname, c.Options, c.SSHAuthorizedKey = p.Hostname, p.Options, p.SSHAuthorizedKey
	if p.WriteFiles {
		// Checked again here: the jail may have started since the
		// request was accepted.
		fresh, err := s.store.Get(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		if fresh.StartedOnce {
			return nil, failure(started(fresh))
		}
		report.Progress("writing rc.conf and root's SSH key into the jail", nil)
		if err := s.jails.ConfigureJail(ctx, c.Pool, c.ID, jailFiles(c)); err != nil {
			return nil, failure(err)
		}
	}
	if err := s.store.UpdateConfig(ctx, c, s.now().UTC()); err != nil {
		return nil, err
	}
	return map[string]any{"container_id": c.ID, "hostname": c.Hostname, "options": c.Options,
		"ssh_key_installed": c.SSHAuthorizedKey != "", "files_written": p.WriteFiles}, nil
}

// DeleteContainer submits the task that destroys a stopped container and
// its dataset.
func (s *Service) DeleteContainer(ctx context.Context, id string, sub Submission) (models.Task, error) {
	if !hostops.ValidID(id) {
		return models.Task{}, fmt.Errorf("container %q: %w", id, models.ErrNotFound)
	}
	refused := sub.request(models.TaskContainerDelete, id, nil)
	c, err := s.store.Get(ctx, id)
	if err != nil {
		return models.Task{}, s.refuse(ctx, refused, err)
	}
	if running, err := s.isRunning(ctx, id); err != nil {
		return models.Task{}, err
	} else if running {
		return models.Task{}, s.refuse(ctx, refused, notStopped(c))
	}
	payload, err := canonical(struct {
		ContainerID string `json:"container_id"`
	}{id})
	if err != nil {
		return models.Task{}, err
	}
	task, _, err := s.engine.Submit(ctx, sub.request(models.TaskContainerDelete, id, payload))
	if err != nil {
		return models.Task{}, err
	}
	return task.Public(), nil
}

// DatasetInfo names a dataset a task destroyed.
type DatasetInfo struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	UsedBytes int64  `json:"used_bytes"`
}

// DeleteResult is the result of container.delete.
type DeleteResult struct {
	ContainerID string        `json:"container_id"`
	Name        string        `json:"name"`
	Destroyed   []DatasetInfo `json:"datasets_destroyed"`
}

// AuditDetails implements tasks.Audited: every dataset destroyed is listed
// (decision 14).
func (r DeleteResult) AuditDetails() map[string]any {
	return map[string]any{"container_id": r.ContainerID, "container_name": r.Name, "datasets_destroyed": r.Destroyed}
}

func (s *Service) runDelete(ctx context.Context, task tasks.Task, report tasks.Reporter) (any, error) {
	c, err := s.store.Get(ctx, task.ResourceID)
	if err != nil {
		return nil, err
	}
	if running, err := s.isRunning(ctx, c.ID); err != nil {
		return nil, err
	} else if running {
		return nil, failure(notStopped(c))
	}
	report.Progress("destroying the jail's dataset", nil)
	destroyed, err := s.jails.DestroyJailRoot(ctx, c.Pool, c.ID)
	if err != nil {
		return nil, failure(err)
	}
	if err := s.jails.RemoveJailRun(c.ID); err != nil {
		return nil, err
	}
	if err := s.store.Delete(ctx, c.ID); err != nil {
		return nil, err
	}
	res := DeleteResult{ContainerID: c.ID, Name: c.Name, Destroyed: []DatasetInfo{}}
	for _, d := range destroyed {
		res.Destroyed = append(res.Destroyed, DatasetInfo{Name: d.Name, Type: d.Type, UsedBytes: d.UsedBytes})
	}
	return res, nil
}
