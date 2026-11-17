// Package containers is the agent's jail service (ADR 0010 decisions 8, 9,
// 12, and 13): it validates what was asked for, turns every write into a
// task, runs those tasks through internal/hostops, and records what it
// observes. It knows nothing about HTTP and runs no command itself.
package containers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/internal/templates"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Page sizes of the container list.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// ConsoleLogMax bounds the console.log tail a reader gets (decision 9).
const ConsoleLogMax = 64 << 10

// nameRE is the shape of a container name, as for templates.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// Engine is what the service needs from the task engine.
type Engine interface {
	Submit(ctx context.Context, req tasks.Request) (tasks.Task, bool, error)
	Refuse(ctx context.Context, req tasks.Request, outcome audit.Outcome, cause *models.Error)
	LookupKey(ctx context.Context, key string) (tasks.Task, bool, error)
}

// Options configures a Service.
type Options struct {
	NodeID    string
	Store     *database.ContainerStore
	Templates *database.TemplateStore
	// Snapshots is the snapshot store shared with the VMs (decision 2d);
	// MaxSnapshots is storage.max_snapshots_per_resource.
	Snapshots    *database.SnapshotStore
	MaxSnapshots int
	Engine       Engine
	// Host gives the confinement, the jail operations, and the host's
	// release (Templates.HostRelease).
	Host hostops.Ops
	// StopTimeout and StartTimeout are containers.stop_timeout_seconds
	// and containers.start_timeout_seconds.
	StopTimeout  time.Duration
	StartTimeout time.Duration
	Logger       *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
}

// Service implements the container operations of the internal API.
type Service struct {
	nodeID       string
	store        *database.ContainerStore
	templates    *database.TemplateStore
	snaps        *database.SnapshotStore
	maxSnapshots int
	engine       Engine
	conf         hostops.Confinement
	jails        hostops.Jails
	zfs          hostops.ZFS
	release      func(context.Context) (string, error)
	stopTimeout  time.Duration
	startTimeout time.Duration
	log          *slog.Logger
	now          func() time.Time
}

// New returns a Service. Register its Definitions before the engine
// starts.
func New(opts Options) (*Service, error) {
	if opts.NodeID == "" || opts.Store == nil || opts.Templates == nil || opts.Snapshots == nil || opts.Engine == nil ||
		opts.Host.Jails == nil || opts.Host.Templates == nil || opts.Host.ZFS == nil {
		return nil, errors.New("containers: a node ID, the container, template, and snapshot stores, a task engine, and the jail, template, and ZFS host operations are required")
	}
	if opts.MaxSnapshots <= 0 {
		opts.MaxSnapshots = models.DefaultMaxSnapshotsPerResource
	}
	if opts.StopTimeout <= 0 {
		opts.StopTimeout = models.DefaultContainerStopTimeoutSeconds * time.Second
	}
	if opts.StartTimeout <= 0 {
		opts.StartTimeout = 120 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{
		nodeID: opts.NodeID, store: opts.Store, templates: opts.Templates, snaps: opts.Snapshots,
		maxSnapshots: opts.MaxSnapshots, engine: opts.Engine,
		conf: opts.Host.Conf, jails: opts.Host.Jails, zfs: opts.Host.ZFS, release: opts.Host.Templates.HostRelease,
		stopTimeout: opts.StopTimeout, startTimeout: opts.StartTimeout, log: opts.Logger, now: opts.Now,
	}, nil
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

func (sub Submission) request(taskType, containerID string, payload []byte) tasks.Request {
	req := tasks.Request{
		Type: taskType, Payload: payload, IdempotencyKey: sub.IdempotencyKey,
		Actor: sub.Actor, PeerUID: sub.PeerUID, RequestID: sub.RequestID, SourceIP: sub.SourceIP,
	}
	if containerID != "" {
		req.ResourceType, req.ResourceID = models.ContainerResource, containerID
	}
	return req
}

// refuse records a submission refused before the engine saw it. An
// invalid request is a failure; anything a rule refused is a denial.
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

// failure turns an error the user can act on into a task failure with its
// code. Anything else stays an internal error, logged.
func failure(err error) error {
	var me *models.Error
	if errors.As(err, &me) {
		return &tasks.Failure{Code: me.Code, Message: me.Message, Details: me.Details}
	}
	return err
}

// spec is the jail.conf input of a container.
func (s *Service) spec(c models.Container, stopTimeout time.Duration) hostops.JailSpec {
	if stopTimeout <= 0 {
		stopTimeout = s.stopTimeout
	}
	return hostops.JailSpec{Pool: c.Pool, ContainerID: c.ID, Hostname: c.Hostname, SysVIPC: c.Options.SysVIPC,
		Mlock: c.Options.Mlock, StartTimeout: s.startTimeout, StopTimeout: stopTimeout, NICs: jailNICs(c),
		Limits: jailLimits(c.Limits)}
}

// Containers lists the containers by name, with their state as the host
// reports it now.
func (s *Service) Containers(ctx context.Context, opts models.ContainerListOptions) (models.ContainerList, error) {
	limit := opts.Limit
	switch {
	case limit == 0:
		limit = DefaultPageSize
	case limit < 0 || limit > MaxPageSize:
		return models.ContainerList{}, models.Invalid(fmt.Sprintf("limit must be between 1 and %d", MaxPageSize))
	}
	list, next, err := s.store.List(ctx, limit, opts.Cursor)
	if err != nil {
		return models.ContainerList{}, err
	}
	if running, err := s.jails.RunningJails(ctx); err != nil {
		s.log.Warn("containers: cannot list the running jails", "error", err)
	} else {
		for i := range list {
			list[i] = s.observe(ctx, list[i], running, false)
		}
	}
	for i := range list {
		list[i] = withInterfaces(list[i])
	}
	return models.ContainerList{Items: list, NextCursor: next}, nil
}

// Container returns one container, with its state as the host reports it
// now.
func (s *Service) Container(ctx context.Context, id string) (models.Container, error) {
	if !hostops.ValidID(id) {
		return models.Container{}, fmt.Errorf("container %q: %w", id, models.ErrNotFound)
	}
	c, err := s.store.Get(ctx, id)
	if err != nil {
		return models.Container{}, err
	}
	running, err := s.jails.RunningJails(ctx)
	if err != nil {
		s.log.Warn("containers: cannot list the running jails", "error", err)
		return withInterfaces(c), nil
	}
	return withInterfaces(s.observe(ctx, c, running, false)), nil
}

// observe compares a container's recorded state with the running jails and
// records a change. A container in the middle of a start or stop is left
// to its task, unless force is set (at agent start, when no task runs).
func (s *Service) observe(ctx context.Context, c models.Container, running map[string]hostops.JailState, force bool) models.Container {
	if !force && (c.State == models.ContainerStarting || c.State == models.ContainerStopping) {
		return c
	}
	state, detail := models.ContainerStopped, ""
	var jid *int
	if j, ok := running[c.ID]; ok {
		state = models.ContainerRunning
		v := j.JID
		jid = &v
	} else if !c.Provisioned {
		state = models.ContainerUnknown
	}
	if state == models.ContainerStopped && c.DesiredState == models.ContainerRunning {
		detail = "the jail is not running although it should be: it was removed outside Skep, or the host restarted"
		// A detail recorded when the jail stopped (a failed start, the
		// start-up sweep) says more; keep it while the jail stays stopped.
		if c.State == models.ContainerStopped && c.StateDetail != "" {
			detail = c.StateDetail
		}
	}
	if state != models.ContainerRunning {
		c = s.clearEpairs(ctx, c)
	}
	same := state == c.State && detail == c.StateDetail && ((jid == nil) == (c.JID == nil)) && (jid == nil || *jid == *c.JID)
	if same {
		return c
	}
	at := s.now().UTC()
	if err := s.store.SaveObserved(ctx, c.ID, state, jid, detail, at); err != nil {
		s.log.Warn("containers: could not record the observed state", "container_id", c.ID, "error", err)
		return c
	}
	c.State, c.JID, c.StateDetail, c.ObservedAt = state, jid, detail, &at
	return c
}

// clearEpairs forgets the observed epairs of a jail that does not run.
// Any that are still on the host are orphans, reported by the interface
// check and removed by the next start, stop, or delete.
func (s *Service) clearEpairs(ctx context.Context, c models.Container) models.Container {
	stale := false
	for _, n := range c.NICs {
		stale = stale || n.HostInterface != ""
	}
	if !stale {
		return c
	}
	if err := s.store.SaveEpairs(ctx, c.ID, nil); err != nil {
		s.log.Warn("containers: could not clear the observed epairs", "container_id", c.ID, "error", err)
		return c
	}
	for i := range c.NICs {
		c.NICs[i].HostInterface = ""
	}
	return c
}

// ObserveAll records the state of every container. The agent calls it
// before it serves its socket: a jail outlives the agent (it is
// persistent), and a restarted agent knows which are running before it
// answers.
func (s *Service) ObserveAll(ctx context.Context) error {
	running, err := s.jails.RunningJails(ctx)
	if err != nil {
		return err
	}
	ids, err := s.store.IDs(ctx)
	if err != nil {
		return err
	}
	swept := 0
	for _, id := range ids {
		c, err := s.store.Get(ctx, id)
		if err != nil {
			s.log.Warn("containers: could not read a container", "container_id", id, "error", err)
			continue
		}
		if _, ok := running[c.ID]; ok && c.State == models.ContainerStarting {
			if s.sweepHalfStarted(ctx, c) {
				swept++
				delete(running, c.ID)
				continue
			}
		}
		c = s.observe(ctx, c, running, true)
		if _, ok := running[c.ID]; !ok && c.Provisioned {
			s.sweepLeftoverDevfs(ctx, c)
		}
	}
	s.log.Info("containers: observed the jails", "containers", len(ids), "running", len(running), "half_started_removed", swept)
	return nil
}

// sweepLeftoverDevfs unmounts a devfs that a removal outside Skep left
// on the root of a jail that does not run (amendment l); hostops logs
// each one it removes. A failure is logged and the start-up goes on: the
// next start or delete of the jail tries again.
func (s *Service) sweepLeftoverDevfs(ctx context.Context, c models.Container) {
	if _, err := s.jails.RemoveLeftoverDevfs(ctx, c.Pool, c.ID); err != nil {
		s.log.Warn("containers: could not check the jail's root for a leftover devfs", "event", "orphaned_devfs_check_failed",
			"container_id", c.ID, "error", err)
	}
}

// sweepHalfStarted removes the jail of a container still recorded
// starting at agent start: the agent died between jail -c and the end of
// /etc/rc, so the /dev guard may not have run (amendment i). The jail is
// removed without exec.stop and its epairs destroyed; the container is
// recorded stopped with a detail that says why. It reports whether the
// jail was removed; if not, the container is observed as usual and the
// failure logged.
func (s *Service) sweepHalfStarted(ctx context.Context, c models.Container) bool {
	if err := s.jails.DiscardJail(ctx, s.spec(c, 0)); err != nil {
		s.log.Error("containers: could not remove a jail an agent restart left half-started", "event", "jail_start_interrupted",
			"container_id", c.ID, "error", err)
		return false
	}
	s.log.Warn("containers: removed a jail an agent restart left half-started", "event", "jail_start_interrupted",
		"container_id", c.ID)
	if err := s.store.SaveEpairs(ctx, c.ID, nil); err != nil {
		s.log.Warn("containers: could not clear the observed epairs", "container_id", c.ID, "error", err)
	}
	detail := "a start was interrupted by an agent restart; the half-started jail was removed without running /etc/rc.shutdown"
	if err := s.store.SaveObserved(ctx, c.ID, models.ContainerStopped, nil, detail, s.now().UTC()); err != nil {
		s.log.Warn("containers: could not record the observed state", "container_id", c.ID, "error", err)
	}
	return true
}

// ConsoleLog returns the end of a jail's console.log (decision 9).
func (s *Service) ConsoleLog(ctx context.Context, id string) (models.ContainerConsoleLog, error) {
	if _, err := s.Container(ctx, id); err != nil {
		return models.ContainerConsoleLog{}, err
	}
	text, err := s.jails.ConsoleLogTail(id, ConsoleLogMax)
	if err != nil {
		return models.ContainerConsoleLog{}, err
	}
	return models.ContainerConsoleLog{Text: text, Truncated: len(text) == ConsoleLogMax}, nil
}

// checkRelease refuses a template whose userland is not the host's
// release (decision 7b): an older one is not offered in this milestone,
// and a newer one never.
func (s *Service) checkRelease(ctx context.Context, t models.Template) error {
	host, err := s.release(ctx)
	if err != nil {
		return err
	}
	hr, herr := templates.ParseRelease(host)
	tr, terr := templates.ParseRelease(t.Release)
	if herr != nil || terr != nil || hr.MajorMinor() != tr.MajorMinor() {
		return &models.Error{Code: models.CodeReleaseUnsupported,
			Message: "the template's userland " + t.Release + " is not the host's release " + host + "; jails need a template of the host's major.minor",
			Details: map[string]any{"template_release": t.Release, "host_release": host}}
	}
	return nil
}
