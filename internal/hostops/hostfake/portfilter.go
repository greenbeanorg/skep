package hostfake

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/internal/portfilter"
	"github.com/greenbeanorg/skep/pkg/models"
)

// fakeFilter is a simulated filter on a port: its ether node and the
// program it carries (nil: the hooks are gone).
type fakeFilter struct {
	ether portfilter.NodeID
	prog  []portfilter.Insn
}

// fakeFilters is the simulated netgraph state of the fake.
type fakeFilters struct {
	// filters by interface name.
	filters map[string]fakeFilter
	// nextNode numbers ether nodes.
	nextNode int
	// failInstall makes InstallPortFilter fail; failReinstall
	// ReinstallPortFilter.
	failInstall, failReinstall bool
	// vlanFiltered and pvid are the bridges' VLAN filtering state.
	vlanFiltered map[string]bool
	pvid         map[string]map[string]int

	lockMu sync.Mutex
	locks  map[string]chan struct{}
}

func (f *Fake) filterState() *fakeFilters {
	if f.pf.filters == nil {
		f.pf.filters = map[string]fakeFilter{}
		f.pf.vlanFiltered = map[string]bool{}
		f.pf.pvid = map[string]map[string]int{}
		f.pf.nextNode = 0x10
	}
	return &f.pf
}

// installFilterLocked records a filter for ifName; f.mu is held.
func (f *Fake) installFilterLocked(ifName string, p portfilter.Params) error {
	prog, err := portfilter.Program(p)
	if err != nil {
		return err
	}
	pf := f.filterState()
	id := portfilter.NodeID(strconv.FormatInt(int64(pf.nextNode), 16))
	pf.nextNode++
	pf.filters[ifName] = fakeFilter{ether: id, prog: prog}
	f.record("ngctl install filter on %s", ifName)
	return nil
}

// fakePortFilters implements hostops.PortFilters for the fake.
type fakePortFilters struct{ f *Fake }

func (p fakePortFilters) PortFilterAvailable(context.Context) error {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	if p.f.NoNetgraph {
		return &models.Error{Code: models.CodePortFilterUnavailable, Message: "the netgraph modules are not loaded (simulated)"}
	}
	return nil
}

// exists reports whether a simulated port of that name exists; f.mu is
// held.
func (f *Fake) portExistsLocked(ifName string) bool {
	_, tap := f.taps[ifName]
	_, epair := f.epairs[ifName]
	return tap || epair
}

func (p fakePortFilters) InstallPortFilter(_ context.Context, ifName string, params portfilter.Params) (portfilter.NodeID, error) {
	f := p.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.portExistsLocked(ifName) {
		return "", &models.Error{Code: models.CodePortFilterFailed, Message: "no such port " + ifName + " (simulated)"}
	}
	if f.filterState().failInstall {
		return "", &models.Error{Code: models.CodePortFilterFailed, Message: "the filter could not be installed on " + ifName + " (simulated)"}
	}
	if err := f.installFilterLocked(ifName, params); err != nil {
		return "", &models.Error{Code: models.CodePortFilterFailed, Message: err.Error()}
	}
	return f.pf.filters[ifName].ether, nil
}

func (p fakePortFilters) InspectPortFilter(_ context.Context, ifName string, params portfilter.Params) (hostops.PortFilterStatus, error) {
	f := p.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.portExistsLocked(ifName) {
		return hostops.PortFilterStatus{Gone: true}, nil
	}
	want, err := portfilter.Program(params)
	if err != nil {
		return hostops.PortFilterStatus{}, err
	}
	pf := f.filterState()
	flt, ok := pf.filters[ifName]
	if !ok {
		// A port without a filter still has an ether node.
		flt = fakeFilter{ether: portfilter.NodeID(strconv.FormatInt(int64(pf.nextNode), 16))}
		pf.nextNode++
		pf.filters[ifName] = flt
	}
	st := hostops.PortFilterStatus{Ether: flt.ether}
	switch {
	case flt.prog == nil:
		st.Problem = "no filter: the lower and upper hooks are not connected"
	case !portfilter.Equal(flt.prog, want):
		st.Problem = "the filter's program is not the one generated for this port"
	}
	return st, nil
}

func (p fakePortFilters) ReinstallPortFilter(_ context.Context, ifName string, ether portfilter.NodeID, params portfilter.Params) error {
	f := p.f
	f.mu.Lock()
	defer f.mu.Unlock()
	pf := f.filterState()
	if flt, ok := pf.filters[ifName]; !ok || flt.ether != ether || !f.portExistsLocked(ifName) {
		return &models.Error{Code: models.CodePortFilterFailed, Message: "the netgraph node is gone (simulated)"}
	}
	if pf.failReinstall {
		return &models.Error{Code: models.CodePortFilterFailed, Message: "the filter could not be reinstalled on " + ifName + " (simulated)"}
	}
	prog, err := portfilter.Program(params)
	if err != nil {
		return err
	}
	pf.filters[ifName] = fakeFilter{ether: ether, prog: prog}
	f.record("ngctl reinstall filter on %s", ifName)
	return nil
}

func (p fakePortFilters) DetachPort(_ context.Context, bridge, ifName string) error {
	f := p.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.taps[ifName]; ok && t.bridge == bridge {
		t.bridge = ""
		f.taps[ifName] = t
	} else if e, ok := f.epairs[ifName]; ok && e.bridge == bridge {
		e.bridge = ""
		f.epairs[ifName] = e
	} else {
		return fmt.Errorf("hostfake: %s is not a member of %s", ifName, bridge)
	}
	f.record("ifconfig %s deletem %s", bridge, ifName)
	return nil
}

func (p fakePortFilters) LockPort(kind hostops.PortOwner, ownerID string, slot int, wait bool) (func(), error) {
	if !hostops.ValidID(ownerID) || slot < 0 || slot > hostops.MaxSlot {
		return nil, errors.New("hostfake: invalid port")
	}
	pf := &p.f.pf
	key := fmt.Sprintf("%s/%s/%d", kind, ownerID, slot)
	for {
		pf.lockMu.Lock()
		if pf.locks == nil {
			pf.locks = map[string]chan struct{}{}
		}
		held, busy := pf.locks[key]
		if !busy {
			ch := make(chan struct{})
			pf.locks[key] = ch
			pf.lockMu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					pf.lockMu.Lock()
					delete(pf.locks, key)
					pf.lockMu.Unlock()
					close(ch)
				})
			}, nil
		}
		pf.lockMu.Unlock()
		if !wait {
			return nil, hostops.ErrPortBusy
		}
		<-held
	}
}

func (p fakePortFilters) BridgeVLAN(ctx context.Context, bridge string) (hostops.BridgeVLAN, error) {
	b, err := p.f.Bridge(ctx, bridge)
	if err != nil || !b.Exists {
		return hostops.BridgeVLAN{}, err
	}
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	pf := p.f.filterState()
	out := hostops.BridgeVLAN{Exists: true, Filtering: pf.vlanFiltered[bridge], PVID: map[string]int{}}
	for m, v := range pf.pvid[bridge] {
		out.PVID[m] = v
	}
	return out, nil
}

// RemoveFilter simulates host root removing a port's filter (fail-open,
// ADR 0011 P2 8.1).
func (f *Fake) RemoveFilter(ifName string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pf := f.filterState()
	flt := pf.filters[ifName]
	flt.prog = nil
	pf.filters[ifName] = flt
}

// FilterInstalled reports whether a port carries a complete filter.
func (f *Fake) FilterInstalled(ifName string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.filterState().filters[ifName].prog != nil
}

// FilterProgram returns the program on a port, nil when it has none.
func (f *Fake) FilterProgram(ifName string) []portfilter.Insn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.filterState().filters[ifName].prog
}

// FailPortFilters makes installs and reinstalls fail.
func (f *Fake) FailPortFilters(install, reinstall bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pf := f.filterState()
	pf.failInstall, pf.failReinstall = install, reinstall
}

// SetBridgeVLAN sets a bridge's simulated VLAN filtering and PVIDs.
func (f *Fake) SetBridgeVLAN(bridge string, filtering bool, pvid map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pf := f.filterState()
	pf.vlanFiltered[bridge] = filtering
	pf.pvid[bridge] = pvid
}

// PortBridge returns the bridge a simulated port is a member of, "" when
// none.
func (f *Fake) PortBridge(ifName string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.taps[ifName]; ok {
		return t.bridge
	}
	return f.epairs[ifName].bridge
}
