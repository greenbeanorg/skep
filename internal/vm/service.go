// Package vm is the agent's virtual machine service: it validates what
// was asked for, turns every write into a task (ADR 0009 decision 9),
// runs those tasks against a hypervisor adapter, and records what the
// agent observes. It knows nothing about HTTP and runs no command itself:
// the adapter does, through internal/hostops.
package vm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/hypervisor"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Page sizes of the VM list.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// Engine is what the service needs from the task engine.
type Engine interface {
	Submit(ctx context.Context, req tasks.Request) (tasks.Task, bool, error)
	Refuse(ctx context.Context, req tasks.Request, outcome audit.Outcome, cause *models.Error)
	LookupKey(ctx context.Context, key string) (tasks.Task, bool, error)
}

// Options configures a Service.
type Options struct {
	NodeID string
	Store  *database.VMStore
	Engine Engine
	// Hypervisor is the bhyve adapter, the only engine there is.
	Hypervisor hypervisor.Hypervisor
	// Host is used for the pool and media listings, and for snapshots and
	// clones, which are storage operations the same for every engine;
	// everything else a VM needs on the host goes through the adapter.
	Host hostops.Ops
	// Snapshots is the snapshot store (ADR 0010 decision 2d).
	Snapshots *database.SnapshotStore
	// MaxSnapshots is storage.max_snapshots_per_resource. 0 means
	// models.DefaultMaxSnapshotsPerResource.
	MaxSnapshots int
	// Console is told what the watcher observes, so a running VM's
	// console is open from the start. Optional.
	Console ConsoleTracker
	Logger  *slog.Logger
	// Now defaults to time.Now. Tests replace it.
	Now func() time.Time
}

// ConsoleTracker keeps the consoles of the running VMs open.
type ConsoleTracker interface {
	Track(vmID string, running bool)
}

// Service implements the VM and storage operations of the internal API.
type Service struct {
	console ConsoleTracker
	nodeID  string
	store   *database.VMStore
	engine  Engine
	hv      hypervisor.Hypervisor
	host    hostops.Ops
	snaps   *database.SnapshotStore
	// maxSnapshots bounds the snapshots of one VM.
	maxSnapshots int
	log          *slog.Logger
	now          func() time.Time
}

// New returns a Service. Register its task definitions with the engine
// (Definitions) before the engine starts.
func New(opts Options) (*Service, error) {
	if opts.NodeID == "" || opts.Store == nil || opts.Snapshots == nil || opts.Engine == nil || opts.Hypervisor == nil {
		return nil, errors.New("vm: a node ID, the VM and snapshot stores, a task engine, and a hypervisor are required")
	}
	if opts.MaxSnapshots <= 0 {
		opts.MaxSnapshots = models.DefaultMaxSnapshotsPerResource
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{
		nodeID: opts.NodeID, store: opts.Store, engine: opts.Engine, hv: opts.Hypervisor,
		host: opts.Host, log: opts.Logger, now: opts.Now, console: opts.Console,
		snaps: opts.Snapshots, maxSnapshots: opts.MaxSnapshots,
	}, nil
}

// Submission is who a write is made for and how it arrived. The actor is
// asserted by skep-api; the peer UID is verified by the kernel.
type Submission struct {
	Actor          models.TaskActor
	PeerUID        uint32
	RequestID      string
	SourceIP       string
	IdempotencyKey string
}

func (sub Submission) request(taskType, vmID string, payload []byte) tasks.Request {
	req := tasks.Request{
		Type: taskType, Payload: payload, IdempotencyKey: sub.IdempotencyKey,
		Actor: sub.Actor, PeerUID: sub.PeerUID, RequestID: sub.RequestID, SourceIP: sub.SourceIP,
	}
	if vmID != "" {
		req.ResourceType, req.ResourceID = models.VMResource, vmID
	}
	return req
}

// refuse records a submission the service refused before the engine saw
// it, and returns the cause. An invalid request is a failure; anything a
// rule refused is a denial (ADR 0005).
func (s *Service) refuse(ctx context.Context, req tasks.Request, err error) error {
	var me *models.Error
	if !errors.As(err, &me) {
		return err
	}
	outcome := audit.Denied
	if me.Code == models.CodeInvalidRequest {
		outcome = audit.Failure
	}
	// The refused request is not stored: a payload is only kept on a task.
	req.Payload, req.Companion = nil, nil
	s.engine.Refuse(ctx, req, outcome, me)
	return err
}

// Capabilities reports what the hypervisor can do on this host.
func (s *Service) Capabilities(ctx context.Context) models.EngineCapabilities {
	return s.hv.Capabilities(ctx)
}

// VMs returns a page of VMs, ordered by name.
func (s *Service) VMs(ctx context.Context, opts models.VMListOptions) (models.VMList, error) {
	switch opts.State {
	case "", models.VMUnknown, models.VMStopped, models.VMStarting, models.VMRunning, models.VMStopping, models.VMFailed:
	default:
		return models.VMList{}, models.Invalid("unknown VM state " + opts.State)
	}
	switch {
	case opts.Limit == 0:
		opts.Limit = DefaultPageSize
	case opts.Limit < 0 || opts.Limit > MaxPageSize:
		return models.VMList{}, models.Invalid(fmt.Sprintf("limit must be between 1 and %d", MaxPageSize))
	}
	list, next, err := s.store.List(ctx, opts)
	if err != nil {
		return models.VMList{}, err
	}
	return models.VMList{Items: list, NextCursor: next}, nil
}

// VM returns one VM.
func (s *Service) VM(ctx context.Context, id string) (models.VM, error) {
	if !hostops.ValidID(id) {
		return models.VM{}, fmt.Errorf("vm %q: %w", id, models.ErrNotFound)
	}
	return s.store.Get(ctx, id)
}

// StoragePools reports the allow-listed pools as they are now, and
// records what it saw.
func (s *Service) StoragePools(ctx context.Context) (models.StoragePoolList, error) {
	pools, err := s.host.Disks().Pools(ctx)
	if err != nil {
		return models.StoragePoolList{}, err
	}
	at := s.now().UTC()
	list := models.StoragePoolList{Items: make([]models.StoragePool, 0, len(pools))}
	for _, p := range pools {
		sp := models.StoragePool{
			Name: p.Name, Type: p.Kind, Capabilities: models.PoolCapabilities(p.Kind), Health: p.Health, Error: p.Error,
			SizeBytes: p.SizeBytes, AllocatedBytes: p.AllocatedBytes, FreeBytes: p.FreeBytes,
			AvailableBytes: p.AvailableBytes, ObservedAt: &at,
		}
		if p.Error != "" {
			sp.Health = "UNAVAILABLE"
		}
		list.Items = append(list.Items, sp)
	}
	if err := s.store.SavePools(ctx, s.nodeID, list.Items, at); err != nil {
		// The answer is what was observed; the cache is a convenience.
		s.log.Warn("vm: could not record the observed pools", "error", err)
	}
	return list, nil
}

// Media lists the install media directory.
func (s *Service) Media(ctx context.Context) (models.MediaList, error) {
	files, err := s.host.Files.Media(ctx)
	if err != nil {
		return models.MediaList{}, err
	}
	list := models.MediaList{Items: make([]models.Media, 0, len(files))}
	for _, f := range files {
		list.Items = append(list.Items, models.Media{Name: f.Name, SizeBytes: f.SizeBytes, ModifiedAt: f.ModifiedAt})
	}
	return list, nil
}

// Observe inspects one VM on the host and records what it saw. It writes
// observed columns only.
func (s *Service) Observe(ctx context.Context, id string) (hypervisor.Observation, error) {
	obs, err := s.hv.Inspect(ctx, id)
	if err != nil {
		return hypervisor.Observation{}, err
	}
	err = s.store.SaveObserved(ctx, id, database.Observation{
		State: obs.State, Detail: obs.Detail, PID: obs.PID, Taps: obs.Taps, At: s.now().UTC(),
	})
	if s.console != nil {
		s.console.Track(id, Running(obs.State))
	}
	return obs, err
}

// Running reports whether an observed state has a guest that runs, and so
// a console.
func Running(state string) bool {
	return state == models.VMRunning || state == models.VMStopping
}

// ObserveAll inspects every VM in the database. The agent calls it before
// it serves its socket, so it knows what is running before it answers.
func (s *Service) ObserveAll(ctx context.Context) error {
	ids, err := s.store.IDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.Observe(ctx, id); err != nil {
			s.log.Warn("vm: could not observe a VM", "vm_id", id, "error", err)
		}
	}
	return nil
}
