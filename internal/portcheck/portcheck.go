// Package portcheck keeps the anti-spoofing filters of running guests'
// ports in place (ADR 0011 decision 5): every 10 seconds, and once at
// agent start before the socket is served, it compares each running
// guest's port with the filter generated for it and reacts to a filter
// that is missing or wrong.
//
// It owns the check and the whole reaction for every port, jail and VM
// alike (addition a): an event and an audit entry, a reinstall, and, when
// the reinstall fails or the same port has failed three checks in a row,
// removing the port from its bridge so the guest is cut off rather than
// unfiltered. It also feeds two health checks: port_filters and
// bridge_vlan_unfiltered.
//
// A port is checked only once its install has completed, which is the
// same as being a member of its bridge, and only under the port's lock,
// taken without waiting: a port whose lock is held (an install is
// running) is skipped for that round. Under the lock the port's owner is
// read again, and the repair addresses the netgraph node by ID.
package portcheck

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/portfilter"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Interval is how often the ports are checked (answer 3). The fail-open
// window of a removed filter is at most this long.
const Interval = 10 * time.Second

// MaxConsecutiveFailures is how many checks in a row the same port may
// fail before it is detached and no longer repaired (answer 3).
const MaxConsecutiveFailures = 3

// Port is a NIC of a running guest, as the database records it.
type Port struct {
	Kind    hostops.PortOwner
	OwnerID string
	Slot    int
	Bridge  string
	Params  portfilter.Params
}

// key identifies a port by owner and slot.
type key struct {
	kind  hostops.PortOwner
	owner string
	slot  int
}

func (p Port) key() key { return key{p.Kind, p.OwnerID, p.Slot} }

// resourceType is the audit resource type of a port's owner.
func (p Port) resourceType() string {
	if p.Kind == hostops.PortVM {
		return "vm"
	}
	return "container"
}

// PortLister lists the ports of every running guest.
type PortLister func(ctx context.Context) ([]Port, error)

// Options configures a Checker.
type Options struct {
	Host hostops.Ops
	// Ports lists what the running guests' ports should be.
	Ports PortLister
	// Audit receives port_filter.missing, .restored and .detached.
	Audit  audit.Writer
	Logger *slog.Logger
}

// portStatus is the last verdict on a port.
type portStatus struct {
	iface    string
	bridge   string
	problem  string // "" when the filter is complete
	detached bool
}

// Checker runs the checks.
type Checker struct {
	opts Options
	log  *slog.Logger
	uid  uint32

	round sync.Mutex // one round at a time

	mu sync.Mutex
	// failures counts consecutive failed checks per ether node ID; a new
	// interface has a new ID and starts at zero.
	failures map[portfilter.NodeID]int
	// given up: ports detached after MaxConsecutiveFailures, by node ID.
	givenUp map[portfilter.NodeID]bool
	status  map[key]portStatus
	// unavailable is set while the netgraph modules are missing.
	unavailable string
	// vlan holds the bridge_vlan_unfiltered problems of the last round.
	vlan    []string
	checked bool
}

// New returns a Checker.
func New(opts Options) (*Checker, error) {
	if opts.Host.Filters == nil || opts.Host.Jails == nil || opts.Host.Net == nil || opts.Ports == nil || opts.Audit == nil {
		return nil, errors.New("portcheck: the host operations, a port lister, and an audit writer are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Checker{opts: opts, log: opts.Logger, uid: uint32(os.Geteuid()),
		failures: map[portfilter.NodeID]int{}, givenUp: map[portfilter.NodeID]bool{}, status: map[key]portStatus{}}, nil
}

// Run checks every Interval until ctx ends. The caller runs one Check
// first, at start.
func (c *Checker) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.Check(ctx); err != nil && ctx.Err() == nil {
				c.log.Error("portcheck: the port filter check failed", "error", err)
			}
		}
	}
}

// Check runs one round over every running guest's port.
func (c *Checker) Check(ctx context.Context) error {
	c.round.Lock()
	defer c.round.Unlock()
	ports, err := c.opts.Ports(ctx)
	if err != nil {
		c.setUnavailable("the guests' ports could not be listed")
		return fmt.Errorf("portcheck: list the guests' ports: %w", err)
	}
	ifaces, err := c.opts.Host.Jails.SkepInterfaces(ctx)
	if err != nil {
		c.setUnavailable("the guest ports could not be listed")
		return fmt.Errorf("portcheck: list the guest ports: %w", err)
	}
	byKey := map[key]string{}
	for _, i := range ifaces {
		kind := hostops.PortJail
		if i.Kind == "tap" {
			kind = hostops.PortVM
		}
		byKey[key{kind, i.OwnerID, i.Slot}] = i.Name
	}

	unavailable := ""
	if err := c.opts.Host.Filters.PortFilterAvailable(ctx); err != nil {
		unavailable = err.Error()
	}
	seen := map[key]bool{}
	bridges := map[string][]string{} // bridge -> guest ports on it
	for _, p := range ports {
		iface, ok := byKey[p.key()]
		if !ok {
			continue // not created yet, or already destroyed
		}
		seen[p.key()] = true
		st, checked := c.checkPort(ctx, p, iface, unavailable != "")
		if !checked {
			continue
		}
		c.mu.Lock()
		c.status[p.key()] = st
		c.mu.Unlock()
		if !st.detached {
			bridges[p.Bridge] = append(bridges[p.Bridge], iface)
		}
	}
	vlan := c.checkVLAN(ctx, bridges)

	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.status {
		if !seen[k] {
			delete(c.status, k)
		}
	}
	c.unavailable, c.vlan, c.checked = unavailable, vlan, true
	return nil
}

func (c *Checker) setUnavailable(why string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unavailable, c.checked = why, true
}

// checkPort checks one port under its lock. checked is false when the
// port was skipped (its lock is held, or it is no longer the owner's).
func (c *Checker) checkPort(ctx context.Context, p Port, iface string, noModules bool) (portStatus, bool) {
	f := c.opts.Host.Filters
	unlock, err := f.LockPort(p.Kind, p.OwnerID, p.Slot, false)
	if errors.Is(err, hostops.ErrPortBusy) {
		return portStatus{}, false
	}
	if err != nil {
		c.log.Error("portcheck: could not lock a port", "interface", iface, "error", err)
		return portStatus{}, false
	}
	defer unlock()

	// Read the port again under the lock: its owner cannot recreate it
	// now, and another owner's interface carries another description.
	owner, ok, err := f.PortOwner(ctx, iface)
	if err != nil || !ok || owner.OwnerID != p.OwnerID || owner.Slot != p.Slot {
		return portStatus{}, false
	}
	st := portStatus{iface: iface, bridge: p.Bridge}
	b, err := c.opts.Host.Net.Bridge(ctx, p.Bridge)
	if err != nil {
		st.problem = "its bridge " + p.Bridge + " could not be read"
		return st, true
	}
	if !contains(b.Members, iface) {
		// Not a member and no install running: it was detached (by this
		// checker, possibly before an agent restart, or by hand).
		st.detached, st.problem = true, "detached from "+p.Bridge+"; the guest has no network on it until it is restarted"
		return st, true
	}
	if noModules {
		st.problem = "unfiltered: the netgraph modules are not loaded"
		return st, true
	}
	ins, err := f.InspectPortFilter(ctx, iface, p.Params)
	switch {
	case err != nil:
		st.problem = "its filter could not be read"
		c.log.Error("portcheck: could not read a port filter", "interface", iface, "error", err)
		return st, true
	case ins.Gone:
		return portStatus{}, false
	case ins.Problem == "":
		c.mu.Lock()
		delete(c.failures, ins.Ether)
		c.mu.Unlock()
		return st, true
	}
	return c.react(ctx, p, iface, ins), true
}

// react handles a failed check (decision 5): event and audit, then a
// reinstall, or a detach when the reinstall fails or the port has failed
// MaxConsecutiveFailures checks in a row.
func (c *Checker) react(ctx context.Context, p Port, iface string, ins hostops.PortFilterStatus) portStatus {
	c.mu.Lock()
	c.failures[ins.Ether]++
	n := c.failures[ins.Ether]
	givenUp := c.givenUp[ins.Ether]
	c.mu.Unlock()

	attrs := []any{"resource_type", p.resourceType(), "resource_id", p.OwnerID, "slot", p.Slot,
		"interface", iface, "bridge", p.Bridge, "problem", ins.Problem, "consecutive_failures", n}
	details := map[string]any{"slot": p.Slot, "interface": iface, "bridge": p.Bridge, "problem": ins.Problem, "consecutive_failures": n}
	c.log.Error("portcheck: a guest port's anti-spoofing filter is missing or wrong", append([]any{"event", models.EventPortFilterMissing}, attrs...)...)
	c.audit(ctx, p, "port_filter.missing", audit.Failure, details)

	if givenUp || n >= MaxConsecutiveFailures {
		reason := fmt.Sprintf("the filter failed %d checks in a row", n)
		return c.detach(ctx, p, iface, ins.Ether, reason, details)
	}
	if err := c.opts.Host.Filters.ReinstallPortFilter(ctx, iface, ins.Ether, p.Params); err != nil {
		details["reinstall_error"] = err.Error()
		return c.detach(ctx, p, iface, ins.Ether, "the filter could not be reinstalled: "+err.Error(), details)
	}
	c.log.Warn("portcheck: reinstalled a guest port's anti-spoofing filter", append([]any{"event", models.EventPortFilterRestored}, attrs...)...)
	c.audit(ctx, p, "port_filter.restored", audit.Success, details)
	return portStatus{iface: iface, bridge: p.Bridge,
		problem: "its filter was missing or wrong and was reinstalled (" + ins.Problem + ")"}
}

// detach removes the port from its bridge and gives up on it until the
// guest is restarted (a new interface, a new node ID).
func (c *Checker) detach(ctx context.Context, p Port, iface string, ether portfilter.NodeID, reason string, details map[string]any) portStatus {
	c.mu.Lock()
	c.givenUp[ether] = true
	c.mu.Unlock()
	details["reason"] = reason
	details["error_code"] = models.CodePortFilterFailed
	st := portStatus{iface: iface, bridge: p.Bridge, detached: true,
		problem: "detached from " + p.Bridge + ": " + reason + "; the guest has no network on it until it is restarted"}
	if err := c.opts.Host.Filters.DetachPort(ctx, p.Bridge, iface); err != nil {
		details["detach_error"] = err.Error()
		st.detached = false
		st.problem = "unfiltered and could not be detached from " + p.Bridge + ": " + err.Error()
		c.log.Error("portcheck: could not detach a guest port whose filter could not be kept",
			"event", models.EventPortFilterDetached, "resource_id", p.OwnerID, "interface", iface, "bridge", p.Bridge, "error", err)
		c.audit(ctx, p, "port_filter.detached", audit.Failure, details)
		return st
	}
	c.log.Error("portcheck: detached a guest port whose filter could not be kept",
		"event", models.EventPortFilterDetached, "resource_type", p.resourceType(), "resource_id", p.OwnerID,
		"slot", p.Slot, "interface", iface, "bridge", p.Bridge, "reason", reason)
	c.audit(ctx, p, "port_filter.detached", audit.Success, details)
	return st
}

func (c *Checker) audit(ctx context.Context, p Port, action string, outcome audit.Outcome, details map[string]any) {
	d := make(map[string]any, len(details))
	for k, v := range details {
		d[k] = v
	}
	err := c.opts.Audit.Write(ctx, audit.Entry{
		ActorID: audit.System, ActorName: audit.System, PeerUID: c.uid,
		Action: action, ResourceType: p.resourceType(), ResourceID: p.OwnerID,
		Phase: audit.Finished, Outcome: outcome, Details: d,
	})
	if err != nil {
		c.log.Error("portcheck: could not write an audit entry", "action", action, "error", err)
	}
}

// checkVLAN finds the bridges with guest ports that do not filter VLANs,
// and guest ports without a PVID (decision 8, answer 6).
func (c *Checker) checkVLAN(ctx context.Context, bridges map[string][]string) []string {
	var problems []string
	names := make([]string, 0, len(bridges))
	for b := range bridges {
		names = append(names, b)
	}
	sort.Strings(names)
	for _, name := range names {
		v, err := c.opts.Host.Filters.BridgeVLAN(ctx, name)
		switch {
		case err != nil:
			problems = append(problems, name+" could not be read")
			continue
		case !v.Exists:
			continue
		case !v.Filtering:
			problems = append(problems, name+" has no VLAN filtering (ifconfig "+name+" vlanfilter)")
			continue
		}
		var missing []string
		for _, port := range bridges[name] {
			if v.PVID[port] == 0 {
				missing = append(missing, port)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			problems = append(problems, name+": guest port(s) without a PVID: "+strings.Join(missing, ", "))
		}
	}
	return problems
}

// Probe is the port_filters health check.
func (c *Checker) Probe(context.Context) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.checked {
		return ""
	}
	var problems []string
	if c.unavailable != "" {
		problems = append(problems, c.unavailable)
	}
	keys := make([]key, 0, len(c.status))
	for k := range c.status {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].owner != keys[j].owner {
			return keys[i].owner < keys[j].owner
		}
		return keys[i].slot < keys[j].slot
	})
	for _, k := range keys {
		st := c.status[k]
		if st.problem != "" {
			problems = append(problems, fmt.Sprintf("%s %s NIC %d (%s): %s", k.kind, k.owner, k.slot, st.iface, st.problem))
		}
	}
	return strings.Join(problems, "; ")
}

// VLANProbe is the bridge_vlan_unfiltered health check.
func (c *Checker) VLANProbe(context.Context) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.vlan, "; ")
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
