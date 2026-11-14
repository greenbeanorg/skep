package hostfake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"syscall"

	"github.com/greenbeanorg/skep/internal/hostops"
)

// Close stops every simulated runner and waits for them.
func (f *Fake) Close() {
	f.stop()
	f.wg.Wait()
}

// CreateTap implements hostops.Net.
func (f *Fake) CreateTap(_ context.Context, vmID string, slot int) (string, error) {
	if !hostops.ValidID(vmID) {
		return "", fmt.Errorf("hostfake: invalid VM ID")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name := fmt.Sprintf("tap%d", f.nextTap)
	f.nextTap++
	f.taps[name] = fakeTap{desc: hostops.TapDescription(vmID, slot)}
	f.record("ifconfig %s create description %s up", name, f.taps[name].desc)
	return name, nil
}

// AddBridgeMember implements hostops.Net.
func (f *Fake) AddBridgeMember(ctx context.Context, bridge, tap string) error {
	b, err := f.Bridge(ctx, bridge)
	if err != nil {
		return err
	}
	if err := hostops.CheckBridgeUsable(f.conf, b); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.taps[tap]
	if !ok {
		return fmt.Errorf("hostfake: no tap %s", tap)
	}
	t.bridge = bridge
	f.taps[tap] = t
	f.record("ifconfig %s addm %s", bridge, tap)
	return nil
}

// DestroyTap implements hostops.Net.
func (f *Fake) DestroyTap(_ context.Context, vmID, tap string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.taps[tap]
	if !ok {
		return nil
	}
	if !strings.HasPrefix(t.desc, "skep:"+vmID+":") {
		return fmt.Errorf("hostfake: refusing to destroy %s: it is not VM %s's", tap, vmID)
	}
	delete(f.taps, tap)
	f.record("ifconfig %s destroy", tap)
	return nil
}

// VMTaps implements hostops.Net.
func (f *Fake) VMTaps(_ context.Context, vmID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for name, t := range f.taps {
		if strings.HasPrefix(t.desc, "skep:"+vmID+":") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Taps returns every simulated tap with its bridge, as "tapN bridge".
func (f *Fake) Taps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for name, t := range f.taps {
		out = append(out, name+" "+t.bridge)
	}
	sort.Strings(out)
	return out
}

// DestroyInstance implements hostops.VMM.
func (f *Fake) DestroyInstance(_ context.Context, vmID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.instances[vmID] {
		delete(f.instances, vmID)
		f.record("bhyvectl --vm=skep-%s --destroy", vmID)
	}
	return nil
}

// ForcePoweroff implements hostops.VMM.
func (f *Fake) ForcePoweroff(_ context.Context, vmID string) error {
	f.mu.Lock()
	g := f.guests[vmID]
	f.mu.Unlock()
	if g != nil {
		g.exit(hostops.ExitStatus{Code: 1})
	}
	return nil
}

// Guest is a simulated bhyve process and its guest.
type Guest struct {
	f      *Fake
	vmID   string
	pid    int
	done   chan struct{}
	status hostops.ExitStatus
	ended  bool
}

// StartBhyve implements hostops.VMM.
func (f *Fake) StartBhyve(_ context.Context, vmID string) (hostops.Process, error) {
	conf, err := f.files.StateFilePath(vmID, hostops.BhyveConfigFile)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.instances[vmID] {
		return nil, fmt.Errorf("hostfake: the instance of VM %s already exists", vmID)
	}
	f.nextPID++
	g := &Guest{f: f, vmID: vmID, pid: f.nextPID, done: make(chan struct{})}
	f.guests[vmID] = g
	f.instances[vmID] = true
	f.record("bhyve -k %s skep-%s", conf, vmID)
	return g, nil
}

// Guest returns the simulated guest of a running VM, or nil.
func (f *Fake) Guest(vmID string) *Guest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.guests[vmID]
}

func (g *Guest) exit(st hostops.ExitStatus) {
	g.f.mu.Lock()
	defer g.f.mu.Unlock()
	if g.ended {
		return
	}
	g.ended, g.status = true, st
	if g.f.guests[g.vmID] == g {
		delete(g.f.guests, g.vmID)
	}
	close(g.done)
}

// Reboot makes the guest reboot: bhyve exits with status 0.
func (g *Guest) Reboot() { g.exit(hostops.ExitStatus{Code: 0}) }

// PowerOff makes the guest power itself off: status 1.
func (g *Guest) PowerOff() { g.exit(hostops.ExitStatus{Code: 1}) }

// Crash makes the guest triple-fault: status 3.
func (g *Guest) Crash() { g.exit(hostops.ExitStatus{Code: 3}) }

// PID implements hostops.Process.
func (g *Guest) PID() int { return g.pid }

// Wait implements hostops.Process.
func (g *Guest) Wait() hostops.ExitStatus {
	<-g.done
	return g.status
}

// Signal implements hostops.Process: SIGTERM is the ACPI power button,
// which the guest obeys unless IgnoreACPI says otherwise; SIGKILL kills.
func (g *Guest) Signal(sig syscall.Signal) error {
	switch sig {
	case syscall.SIGTERM:
		g.f.mu.Lock()
		ignore := g.f.IgnoreACPI[g.vmID]
		g.f.mu.Unlock()
		if !ignore {
			g.exit(hostops.ExitStatus{Code: 1})
		}
	case syscall.SIGKILL:
		g.exit(hostops.ExitStatus{Code: -1, Signal: "killed"})
	}
	return nil
}

// StartRunner implements hostops.Procs: the runner loop runs in a
// goroutine against the fake.
func (f *Fake) StartRunner(_ context.Context, vmID string) (int, error) {
	if f.opts.Runner == nil {
		return 0, fmt.Errorf("hostfake: no runner is configured")
	}
	if err := f.files.PrepareRun(f.ctx, vmID); err != nil {
		return 0, err
	}
	f.mu.Lock()
	if _, ok := f.runners[vmID]; ok {
		f.mu.Unlock()
		return 0, fmt.Errorf("hostfake: VM %s already has a runner", vmID)
	}
	f.nextPID++
	ctx, kill := context.WithCancel(f.ctx)
	r := &fakeRunner{pid: f.nextPID, signals: make(chan hostops.RunnerSignal, 4), kill: kill}
	f.runners[vmID] = r
	f.record("runner start %s", vmID)
	f.mu.Unlock()
	if err := f.files.WriteRunFile(vmID, hostops.RunnerPIDFile, []byte(fmt.Sprintf("%d\n", r.pid))); err != nil {
		return 0, err
	}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		defer kill()
		err := f.opts.Runner(ctx, f.Ops(), vmID, r.pid, r.signals)
		if err != nil && f.opts.Logger != nil {
			f.opts.Logger.Warn("hostfake: the simulated runner ended with an error", "vm_id", vmID, "error", err)
		}
		f.mu.Lock()
		if f.runners[vmID] == r {
			delete(f.runners, vmID)
		}
		f.mu.Unlock()
	}()
	return r.pid, nil
}

// Runner implements hostops.Procs.
func (f *Fake) Runner(vmID string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runners[vmID]
	if !ok {
		return 0, false
	}
	return r.pid, true
}

// SignalRunner implements hostops.Procs.
func (f *Fake) SignalRunner(vmID string, sig hostops.RunnerSignal) error {
	f.mu.Lock()
	r, ok := f.runners[vmID]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("hostfake: VM %s has no runner", vmID)
	}
	select {
	case r.signals <- sig:
		return nil
	default:
		return fmt.Errorf("hostfake: the runner of VM %s is not taking signals", vmID)
	}
}

// KillRunner makes a runner vanish as kill -9 would, leaving its guest
// and instance behind: the runner's context is cancelled, and a runner
// treats that as its own death, cleaning up nothing.
func (f *Fake) KillRunner(vmID string) {
	f.mu.Lock()
	r := f.runners[vmID]
	delete(f.runners, vmID)
	f.mu.Unlock()
	if r != nil {
		r.kill()
	}
}
