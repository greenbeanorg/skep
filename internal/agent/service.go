// Package agent holds the host agent's business logic: it combines node
// identity, host discovery, and health checks into the documents the API
// serves. It knows nothing about HTTP, and the HTTP layer knows nothing
// about discovery, so the API can move into skep-api later without a
// rewrite of either side.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/greenbeanorg/skep/internal/hardware"
	"github.com/greenbeanorg/skep/pkg/models"
	"github.com/greenbeanorg/skep/pkg/version"
)

// Pinger checks that a dependency is reachable. *sql.DB satisfies it.
type Pinger interface {
	PingContext(ctx context.Context) error
}

// Options configures a Service.
type Options struct {
	// NodeID is the stable ID of the local node.
	NodeID string
	// DevMode is reported in the system document.
	DevMode bool
	// Discoverer performs host discovery.
	Discoverer hardware.Discoverer
	// Database is pinged by the health check. Optional.
	Database Pinger
	// Nodes stores what discovery observed. Optional.
	Nodes NodeStore
	// Tasks reads tasks for the API. Optional; without it there are none.
	Tasks TaskReader
	// Checks are additional, non-critical health checks, by name: a
	// failure degrades the document but does not make the agent
	// unavailable. For example the audit
	// archive's. Optional.
	Checks map[string]models.HealthProbe
	// Bhyve reports what the bhyve engine can do on this host, for the
	// system document. Optional.
	Bhyve func(ctx context.Context) models.EngineCapabilities
	// Containers reports what jails can have on this host, for the
	// system document. Optional.
	Containers func(ctx context.Context) (models.ContainerCapabilities, error)
	// CacheTTL is how long a discovery result is reused. Zero disables
	// caching.
	CacheTTL time.Duration
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Now defaults to time.Now. Tests replace it.
	Now func() time.Time
}

// Service implements the agent's read-only operations.
type Service struct {
	nodeID   string
	devMode  bool
	disc     hardware.Discoverer
	db       Pinger
	nodes    NodeStore
	tasks    TaskReader
	checks   map[string]models.HealthProbe
	bhyve    func(ctx context.Context) models.EngineCapabilities
	ctrCaps  func(ctx context.Context) (models.ContainerCapabilities, error)
	cacheTTL time.Duration
	log      *slog.Logger
	now      func() time.Time
	started  time.Time

	// mu serialises discovery so concurrent requests share one pass.
	mu        sync.Mutex
	cached    models.Hardware
	cachedAt  time.Time
	haveCache bool
}

// NewService validates opts and returns a Service.
func NewService(opts Options) (*Service, error) {
	if opts.NodeID == "" {
		return nil, fmt.Errorf("agent: node ID must not be empty")
	}
	if opts.Discoverer == nil {
		return nil, fmt.Errorf("agent: a discoverer is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{
		nodeID:   opts.NodeID,
		devMode:  opts.DevMode,
		disc:     opts.Discoverer,
		db:       opts.Database,
		nodes:    opts.Nodes,
		tasks:    opts.Tasks,
		checks:   opts.Checks,
		bhyve:    opts.Bhyve,
		ctrCaps:  opts.Containers,
		cacheTTL: opts.CacheTTL,
		log:      opts.Logger,
		now:      opts.Now,
		started:  opts.Now().UTC(),
	}, nil
}

// NodeID returns the stable ID of the local node.
func (s *Service) NodeID() string { return s.nodeID }

// Health reports whether the agent and its dependencies are working.
func (s *Service) Health(ctx context.Context) models.Health {
	h := models.Health{
		Status:        models.HealthOK,
		Version:       version.Version,
		UptimeSeconds: s.uptime(),
		Checks:        map[string]models.HealthCheck{},
	}
	if s.db != nil {
		// Critical: without its database the agent can run no task and
		// read no history.
		problem := ""
		if err := s.db.PingContext(ctx); err != nil {
			s.log.Error("health: database check failed", "error", err)
			// The cause is logged, not returned: health is readable
			// without authentication.
			problem = "database is not reachable"
		}
		h.Record("database", true, problem)
	}
	// The additional checks are not critical: a problem degrades the
	// document but the agent still serves requests.
	for name, probe := range s.checks {
		h.Record(name, false, probe(ctx))
	}
	return h
}

// System describes the local node.
func (s *Service) System(ctx context.Context) (models.System, error) {
	hw, err := s.discover(ctx)
	if err != nil {
		return models.System{}, err
	}
	sys := models.System{
		NodeID:         s.nodeID,
		Hostname:       hw.Host.Hostname,
		Platform:       hw.Platform,
		ProductionHost: hw.Platform == models.PlatformFreeBSD && !s.devMode,
		DevMode:        s.devMode,
		OS:             hw.Host.OS,
		Architecture:   hw.Host.Architecture,
		Skep:           version.Get(),
		Capabilities: models.CapabilitySummary{
			Bhyve: hw.Hypervisors.Bhyve.Capability,
			QEMU:  hw.Hypervisors.QEMU.Capability,
			ZFS:   hw.Storage.ZFS.Capability,
		},
		StartedAt:     s.started,
		UptimeSeconds: s.uptime(),
	}
	if s.bhyve != nil {
		// The detail behind Capabilities.Bhyve: what a VM is checked
		// against, so a client can explain a refusal before it happens.
		caps := s.bhyve(ctx)
		sys.Bhyve = &caps
	}
	if s.ctrCaps != nil {
		if caps, err := s.ctrCaps(ctx); err != nil {
			s.log.Warn("agent: cannot read the jail capabilities", "error", err)
		} else {
			sys.Containers = &caps
		}
	}
	return sys, nil
}

// Hardware returns the discovered hardware of a node. Only the local node
// is known in this release; any other ID yields models.ErrNotFound.
func (s *Service) Hardware(ctx context.Context, nodeID string) (models.Hardware, error) {
	if nodeID != s.nodeID {
		return models.Hardware{}, fmt.Errorf("node %q: %w", nodeID, models.ErrNotFound)
	}
	return s.discover(ctx)
}

func (s *Service) uptime() int64 {
	return int64(s.now().Sub(s.started) / time.Second)
}

// discover returns a cached result when it is fresh enough, and otherwise
// runs one discovery pass.
func (s *Service) discover(ctx context.Context) (models.Hardware, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.haveCache && s.now().Sub(s.cachedAt) < s.cacheTTL {
		return s.cached, nil
	}
	return s.discoverLocked(ctx)
}

// discoverLocked runs one discovery pass and replaces the cache. The
// caller holds s.mu.
func (s *Service) discoverLocked(ctx context.Context) (models.Hardware, error) {
	start := s.now()
	hw, err := s.disc.Discover(ctx)
	if err != nil {
		return models.Hardware{}, fmt.Errorf("agent: host discovery: %w", err)
	}
	hw.NodeID = s.nodeID
	s.log.Debug("host discovery finished",
		"duration", s.now().Sub(start),
		"platform", hw.Platform,
		"warnings", len(hw.Warnings))
	for _, w := range hw.Warnings {
		s.log.Warn("host discovery warning", "warning", w)
	}
	s.cached, s.cachedAt, s.haveCache = hw, s.now(), true
	return hw, nil
}
