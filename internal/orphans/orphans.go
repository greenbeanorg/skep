// Package orphans finds the host interfaces Skep made whose owner is not
// running (ADR 0010 decision 10): a jail's epair or a VM's tap left by an
// agent that died between removing its owner and destroying it. They are
// reported, as a warning event at agent start and as the
// orphaned_interfaces health check, and destroyed by the owner's next
// start, stop, or delete; this package destroys nothing.
package orphans

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// DefaultCacheTTL bounds how often the health check lists the interfaces.
const DefaultCacheTTL = 15 * time.Second

// Owner describes the owner of an interface as the database knows it.
// Busy is true while a task starts or stops it: its interfaces may exist
// before (or after) the jail or VM does.
type Owner struct {
	Exists bool
	Busy   bool
}

// OwnerFunc looks up an owner by ID.
type OwnerFunc func(ctx context.Context, id string) (Owner, error)

// Options configures a Checker.
type Options struct {
	Jails hostops.Jails
	VMM   hostops.VMM
	// Container and VM look up the owners of epairs and taps.
	Container OwnerFunc
	VM        OwnerFunc
	CacheTTL  time.Duration
	Logger    *slog.Logger
	Now       func() time.Time
}

// Orphan is an interface whose owner is not running.
type Orphan struct {
	Interface string `json:"interface"`
	// Kind is "epair" (a jail's) or "tap" (a VM's).
	Kind    string `json:"kind"`
	OwnerID string `json:"owner_id"`
	Slot    int    `json:"slot"`
	// OwnerExists is false when the jail or VM no longer exists: the
	// interface is then reported until someone removes it by hand.
	OwnerExists bool `json:"owner_exists"`
}

// Checker finds orphaned interfaces.
type Checker struct {
	opts Options

	mu       sync.Mutex
	cachedAt time.Time
	cached   string
}

// New returns a Checker.
func New(opts Options) (*Checker, error) {
	if opts.Jails == nil || opts.VMM == nil || opts.Container == nil || opts.VM == nil {
		return nil, errors.New("orphans: the jail and vmm operations and both owner lookups are required")
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = DefaultCacheTTL
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Checker{opts: opts}, nil
}

// Find lists the orphaned interfaces now. An interface whose owner is
// being started or stopped is not one.
func (c *Checker) Find(ctx context.Context) ([]Orphan, error) {
	ifaces, err := c.opts.Jails.SkepInterfaces(ctx)
	if err != nil {
		return nil, err
	}
	if len(ifaces) == 0 {
		return nil, nil
	}
	running, err := c.opts.Jails.RunningJails(ctx)
	if err != nil {
		return nil, err
	}
	var out []Orphan
	for _, i := range ifaces {
		var (
			lookup OwnerFunc
			alive  bool
		)
		switch i.Kind {
		case "epair":
			_, alive = running[i.OwnerID]
			lookup = c.opts.Container
		case "tap":
			alive = c.opts.VMM.InstanceExists(i.OwnerID)
			lookup = c.opts.VM
		default:
			continue
		}
		if alive {
			continue
		}
		owner, err := lookup(ctx, i.OwnerID)
		if err != nil {
			return nil, err
		}
		if owner.Busy {
			continue
		}
		out = append(out, Orphan{Interface: i.Name, Kind: i.Kind, OwnerID: i.OwnerID, Slot: i.Slot, OwnerExists: owner.Exists})
	}
	return out, nil
}

// Report logs every orphaned interface as a warning event, and one line
// with the count, so a check that found nothing is visible too. The agent
// calls it at start, before it serves its socket.
func (c *Checker) Report(ctx context.Context) error {
	found, err := c.Find(ctx)
	if err != nil {
		c.opts.Logger.Error("orphans: could not check the host interfaces", "event", "orphaned_interface_check_failed", "error", err)
		return err
	}
	for _, o := range found {
		next := "it is destroyed by the owner's next start, stop, or delete"
		if !o.OwnerExists {
			next = "its owner no longer exists; Skep reports it until it is removed by hand"
		}
		c.opts.Logger.Warn("orphans: an interface Skep made belongs to a jail or VM that is not running; "+next,
			"event", "orphaned_interface", "interface", o.Interface, "kind", o.Kind, "owner_id", o.OwnerID,
			"slot", o.Slot, "owner_exists", o.OwnerExists)
	}
	c.opts.Logger.Info("orphans: interface check finished", "event", "orphaned_interface_check", "found", len(found))
	return nil
}

// Probe is the orphaned_interfaces health check. It names no interface:
// the health document is readable without authentication, and the agent
// log has the details.
func (c *Checker) Probe(ctx context.Context) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.opts.Now()
	if !c.cachedAt.IsZero() && now.Sub(c.cachedAt) < c.opts.CacheTTL {
		return c.cached
	}
	found, err := c.Find(ctx)
	switch {
	case err != nil:
		c.opts.Logger.Warn("orphans: could not check the host interfaces", "error", err)
		c.cached = "could not list the host interfaces"
	case len(found) > 0:
		c.cached = fmt.Sprintf("%d interface(s) with a skep: description belong to a jail or VM that is not running; see the agent log", len(found))
	default:
		c.cached = ""
	}
	c.cachedAt = now
	return c.cached
}

// ContainerOwner adapts a container lookup.
func ContainerOwner(get func(ctx context.Context, id string) (models.Container, error)) OwnerFunc {
	return func(ctx context.Context, id string) (Owner, error) {
		ct, err := get(ctx, id)
		if errors.Is(err, models.ErrNotFound) {
			return Owner{}, nil
		}
		if err != nil {
			return Owner{}, err
		}
		busy := ct.State == models.ContainerStarting || ct.State == models.ContainerStopping
		return Owner{Exists: true, Busy: busy}, nil
	}
}

// VMOwner adapts a VM lookup.
func VMOwner(get func(ctx context.Context, id string) (models.VM, error)) OwnerFunc {
	return func(ctx context.Context, id string) (Owner, error) {
		vm, err := get(ctx, id)
		if errors.Is(err, models.ErrNotFound) {
			return Owner{}, nil
		}
		if err != nil {
			return Owner{}, err
		}
		st := vm.Observed.State
		return Owner{Exists: true, Busy: st == models.VMStarting || st == models.VMStopping}, nil
	}
}
