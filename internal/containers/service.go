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
	"sync"
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
	// MaxLockedMemory is containers.max_locked_memory_bytes, 0 for the
	// default; PhysicalMemory is hw.physmem as the agent read it at
	// start, which the default is a quarter of (amendment o, SR-04).
	MaxLockedMemory int64
	PhysicalMemory  int64
	Logger          *slog.Logger
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
	filters      hostops.PortFilters
	zfs          hostops.ZFS
	release      func(context.Context) (string, error)
	stopTimeout  time.Duration
	startTimeout time.Duration
	// maxLocked is the total locked_memory of all running jails; locked
	// guards lockedStarting, the locked_memory of the starts under way
	// (amendment o, SR-04).
	maxLocked      int64
	locked         sync.Mutex
	lockedStarting map[string]int64
	log            *slog.Logger
	now            func() time.Time
}

// New returns a Service. Register its Definitions before the engine
// starts.
func New(opts Options) (*Service, error) {
	if opts.NodeID == "" || opts.Store == nil || opts.Templates == nil || opts.Snapshots == nil || opts.Engine == nil ||
		opts.Host.Jails == nil || opts.Host.Templates == nil || opts.Host.ZFS == nil || opts.Host.Filters == nil {
		return nil, errors.New("containers: a node ID, the container, template, and snapshot stores, a task engine, and the jail, template, ZFS, and port filter host operations are required")
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
		conf: opts.Host.Conf, jails: opts.Host.Jails, filters: opts.Host.Filters, zfs: opts.Host.ZFS, release: opts.Host.Templates.HostRelease,
		stopTimeout: opts.StopTimeout, startTimeout: opts.StartTimeout, log: opts.Logger, now: opts.Now,
		maxLocked: MaxLockedMemory(opts.MaxLockedMemory, opts.PhysicalMemory), lockedStarting: map[string]int64{},
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

// runningAgainstStopped is the detail of a jail observed running while its
// desired state is stopped, when no failure detail says why.
const runningAgainstStopped = "the jail is running although it should be stopped; stop it again"

// observe compares a container's recorded state with the running jails and
// records a change. A container in the middle of a start or stop is left
// to its task, unless force is set (at agent start, when no task runs).
func (s *Service) observe(ctx context.Context, c models.Container, running map[string]hostops.JailState, force bool) models.Container {
	if !force && (c.State == models.ContainerStarting || c.State == models.ContainerStopping) {
		return c
	}
	// A quarantined jail stays failed, running or not, until a delete or
	// the start-up sweep removes it (amendment n, SR-06): observing it as
	// running would offer a stop, which runs its /etc/rc.shutdown.
	if c.State == models.ContainerFailed {
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
	}
	// A detail recorded when the jail stopped (a failed start, a start-up
	// sweep, a stop completed at agent start) says more; keep it while the
	// jail stays stopped.
	if state == models.ContainerStopped && c.State == models.ContainerStopped && c.StateDetail != "" {
		detail = c.StateDetail
	}
	// A jail that runs although its desired state is stopped. After a
	// failed stop task or a failed stop completion at agent start the
	// recorded state is unknown (and running from the first read on): the
	// detail recorded with the failure says why, and is kept on every read.
	// A jail recorded stopped and now running was started outside Skep;
	// any detail it has is from when it stopped, so the generic sentence is
	// shown. The state shown is the observed one. A container in the
	// middle of its own start or stop is not judged here.
	if state == models.ContainerRunning && c.DesiredState == models.ContainerStopped &&
		c.State != models.ContainerStarting && c.State != models.ContainerStopping {
		detail = runningAgainstStopped
		if (c.State == models.ContainerUnknown || c.State == models.ContainerRunning) && c.StateDetail != "" {
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
	swept, completed := 0, 0
	for _, id := range ids {
		c, err := s.store.Get(ctx, id)
		if err != nil {
			s.log.Warn("containers: could not read a container", "container_id", id, "error", err)
			continue
		}
		if c.State == models.ContainerFailed {
			if s.sweepQuarantined(ctx, c) {
				swept++
				delete(running, c.ID)
				s.sweepLeftoverDevfs(ctx, c)
			}
			continue
		}
		if c.State == models.ContainerStopping {
			// A stop interrupted by an agent restart: finish it
			// (amendment aa). The jail may run with its epairs already
			// destroyed, or be gone with its epairs left.
			if s.completeStop(ctx, c) {
				completed++
			}
			delete(running, c.ID)
			continue
		}
		if _, ok := running[c.ID]; ok && c.State == models.ContainerStarting {
			if s.sweepHalfStarted(ctx, c) {
				swept++
				delete(running, c.ID)
				continue
			}
			// Not removed: quarantined, never observed running, so
			// nothing offers a stop that would run its /etc/rc.shutdown.
			s.quarantine(ctx, c, "a start interrupted by an agent restart left the jail, and removing it failed; "+
				"it is quarantined until a delete or the next agent start removes it")
			delete(running, c.ID)
			continue
		}
		c = s.observe(ctx, c, running, true)
		if _, ok := running[c.ID]; !ok && c.Provisioned {
			s.sweepLeftoverDevfs(ctx, c)
		}
	}
	s.log.Info("containers: observed the jails", "containers", len(ids), "running", len(running), "removed_without_exec_stop", swept,
		"stops_completed", completed)
	return nil
}

// completeStop finishes, at agent start, the stop of a container still
// recorded stopping (amendment aa): the stop task was interrupted by an
// agent restart, after StopJail destroyed the epairs and perhaps before
// jail -r finished. It runs StopJail again with the default stop timeout
// (exec.stop, as a stop does; a jail already gone only has its epairs
// destroyed) and records the container stopped with no epairs. It
// reports whether the stop was completed.
//
// StopJail destroys only the epair host ends whose description, read from
// the host at that moment, is skep:<container-id>:<slot>; it never uses
// the host_interface names recorded on the container, which completeStop
// only clears.
//
// On a failure the container is recorded unknown, as a failed stop task
// records it, and the failure logged. When ctx was cancelled (the agent
// is shutting down) and StopJail failed, nothing is recorded: the
// container stays stopping and the next agent start tries again.
func (s *Service) completeStop(ctx context.Context, c models.Container) bool {
	spec := s.spec(c, 0)
	s.log.Warn("containers: completing a stop an agent restart interrupted; the agent's socket waits for it",
		"event", "jail_stop_completing", "container_id", c.ID,
		"max_wait_seconds", int(spec.StopMaxWait()/time.Second))
	err := s.jails.StopJail(ctx, spec)
	if err != nil && ctx.Err() != nil {
		s.log.Warn("containers: the agent is shutting down during a stop completion; the container stays stopping and the next agent start completes it",
			"event", "jail_stop_completion_cancelled", "container_id", c.ID, "error", err)
		return false
	}
	// Recorded on its own context, as a stop task records: what the host
	// did must be recorded, even while the agent shuts down.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err != nil {
		s.log.Error("containers: could not complete a stop an agent restart interrupted", "event", "jail_stop_interrupted",
			"container_id", c.ID, "error", err)
		detail := "a stop interrupted by an agent restart could not be completed at agent start: " + err.Error() + "; stop it again"
		if serr := s.store.SaveObserved(rctx, c.ID, models.ContainerUnknown, nil, detail, s.now().UTC()); serr != nil {
			s.log.Warn("containers: could not record the observed state", "container_id", c.ID, "error", serr)
		}
		return false
	}
	s.log.Warn("containers: completed a stop an agent restart interrupted", "event", "jail_stop_completed",
		"container_id", c.ID)
	if err := s.store.SaveEpairs(rctx, c.ID, nil); err != nil {
		s.log.Warn("containers: could not clear the observed epairs", "container_id", c.ID, "error", err)
	}
	detail := "a stop interrupted by an agent restart was completed at agent start"
	if err := s.store.SaveObserved(rctx, c.ID, models.ContainerStopped, nil, detail, s.now().UTC()); err != nil {
		s.log.Warn("containers: could not record the observed state", "container_id", c.ID, "error", err)
	}
	s.sweepLeftoverDevfs(rctx, c)
	return true
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

// sweepQuarantined retries, at agent start, the removal of a quarantined
// jail without exec.stop (amendment n, SR-06). Removed, the container is
// recorded stopped; otherwise it stays quarantined and the failure is
// logged. It reports whether the jail was removed.
func (s *Service) sweepQuarantined(ctx context.Context, c models.Container) bool {
	if err := s.jails.DiscardJail(ctx, s.spec(c, 0)); err != nil {
		s.log.Error("containers: could not remove a quarantined jail", "event", "jail_quarantined",
			"container_id", c.ID, "error", err)
		return false
	}
	s.log.Warn("containers: removed a quarantined jail without running any of its code", "event", "jail_quarantine_removed",
		"container_id", c.ID)
	if err := s.store.SaveEpairs(ctx, c.ID, nil); err != nil {
		s.log.Warn("containers: could not clear the observed epairs", "container_id", c.ID, "error", err)
	}
	detail := "a quarantined jail (refused before any of its code ran) was removed at agent start without running /etc/rc.shutdown"
	if err := s.store.SaveObserved(ctx, c.ID, models.ContainerStopped, nil, detail, s.now().UTC()); err != nil {
		s.log.Warn("containers: could not record the observed state", "container_id", c.ID, "error", err)
	}
	return true
}

// quarantine records a container failed (amendment n, SR-06).
func (s *Service) quarantine(ctx context.Context, c models.Container, detail string) {
	s.log.Error("containers: a jail is quarantined", "event", "jail_quarantined", "container_id", c.ID, "detail", detail)
	if err := s.store.SaveObserved(ctx, c.ID, models.ContainerFailed, nil, detail, s.now().UTC()); err != nil {
		s.log.Error("containers: could not record a quarantined jail", "container_id", c.ID, "error", err)
	}
}

// ConsoleLog returns the end of a jail's console.log (decision 9), with
// format and control characters other than newline and tab removed
// (amendment r). The file itself is left as the jail wrote it.
func (s *Service) ConsoleLog(ctx context.Context, id string) (models.ContainerConsoleLog, error) {
	if _, err := s.Container(ctx, id); err != nil {
		return models.ContainerConsoleLog{}, err
	}
	text, err := s.jails.ConsoleLogTail(id, ConsoleLogMax)
	if err != nil {
		return models.ContainerConsoleLog{}, err
	}
	// Truncated is about the file: measured before the cleaning.
	return models.ContainerConsoleLog{Text: hostops.CleanText(text), Truncated: len(text) == ConsoleLogMax}, nil
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
