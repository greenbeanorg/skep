package hostfake

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// fakeJails simulates jails: the jail roots are datasets in the fake's
// pools, a running jail is a JID in memory, and the files Skep would write
// are kept for tests. Every spec goes through the real jail.conf renderer
// and every file set through the real check.
type fakeJails struct{ f *Fake }

func (j fakeJails) EnsureJailsParent(_ context.Context, pool string) error {
	name, err := j.f.conf.JailsDataset(pool)
	if err != nil {
		return err
	}
	f := j.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.zfsLoaded(); err != nil {
		return err
	}
	if _, ok := f.datasets[name]; !ok {
		f.txg++
		f.datasets[name] = hostops.Dataset{Name: name, Type: "filesystem", CreateTxg: f.txg}
		f.record("zfs create %s", name)
	}
	return nil
}

func (j fakeJails) CheckJailsParent(ctx context.Context, pool string) error {
	return j.EnsureJailsParent(ctx, pool)
}

func (j fakeJails) CreateJailRoot(_ context.Context, pool, templateID, containerID string) (hostops.Dataset, error) {
	ds, err := j.f.conf.JailDataset(pool, containerID)
	if err != nil {
		return hostops.Dataset{}, err
	}
	tds, err := j.f.conf.TemplateDataset(pool, templateID)
	if err != nil {
		return hostops.Dataset{}, err
	}
	f := j.f
	f.mu.Lock()
	defer f.mu.Unlock()
	origin := tds + "@" + hostops.BaseSnapshot
	snap, ok := f.snaps[origin]
	if !ok || snap.TemplateID != templateID {
		return hostops.Dataset{}, &models.Error{Code: models.CodeTemplateNotProvisioned,
			Message: "the template has no " + hostops.BaseSnapshot + " snapshot carrying its ID"}
	}
	if _, ok := f.datasets[ds]; ok {
		return hostops.Dataset{}, fmt.Errorf("hostfake: the jail dataset %s already exists", ds)
	}
	f.txg++
	d := hostops.Dataset{Name: ds, Type: "filesystem", ContainerID: containerID, Origin: origin, CreateTxg: f.txg}
	f.datasets[ds] = d
	snap.Clones = append(snap.Clones, ds)
	f.snaps[origin] = snap
	f.record("zfs clone %s %s", origin, ds)
	return d, nil
}

// root is the simulated path of a jail root.
func (j fakeJails) root(pool, containerID string) (string, string, error) {
	ds, err := j.f.conf.JailDataset(pool, containerID)
	if err != nil {
		return "", "", err
	}
	return ds, "/simulated/" + ds, nil
}

func (j fakeJails) JailRoot(_ context.Context, pool, containerID string) (string, error) {
	ds, root, err := j.root(pool, containerID)
	if err != nil {
		return "", err
	}
	f := j.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.datasets[ds]; !ok || d.ContainerID != containerID {
		return "", fmt.Errorf("jail dataset %s: %w", ds, models.ErrNotFound)
	}
	return root, nil
}

func (j fakeJails) DestroyJailRoot(_ context.Context, pool, containerID string) ([]hostops.Dataset, error) {
	ds, _, err := j.root(pool, containerID)
	if err != nil {
		return nil, err
	}
	f := j.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.jails[containerID]; ok {
		return nil, &models.Error{Code: models.CodeContainerNotStopped, Message: "the jail is running"}
	}
	d, ok := f.datasets[ds]
	if !ok {
		return nil, nil
	}
	if d.ContainerID != containerID {
		return nil, fmt.Errorf("hostfake: refusing %s: it does not name container %s", ds, containerID)
	}
	out := []hostops.Dataset{d}
	for name, sn := range f.snaps {
		if strings.HasPrefix(name, ds+"@") {
			out = append(out, sn)
			delete(f.snaps, name)
		}
	}
	delete(f.datasets, ds)
	if d.Origin != "" {
		if origin, ok := f.snaps[d.Origin]; ok {
			var keep []string
			for _, c := range origin.Clones {
				if c != ds {
					keep = append(keep, c)
				}
			}
			origin.Clones = keep
			f.snaps[d.Origin] = origin
		}
	}
	delete(f.jailFiles, containerID)
	f.record("zfs destroy -r %s", ds)
	return out, nil
}

func (j fakeJails) ConfigureJail(_ context.Context, pool, containerID string, files hostops.JailFiles) error {
	if err := files.Check(); err != nil {
		return err
	}
	ds, _, err := j.root(pool, containerID)
	if err != nil {
		return err
	}
	f := j.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.jails[containerID]; ok {
		return &models.Error{Code: models.CodeContainerNotStopped, Message: "the jail is running"}
	}
	if _, ok := f.datasets[ds]; !ok {
		return fmt.Errorf("jail dataset %s: %w", ds, models.ErrNotFound)
	}
	f.jailFiles[containerID] = hostops.JailFiles{RCConf: append([]string(nil), files.RCConf...), AuthorizedKey: files.AuthorizedKey,
		Nameservers: append([]string(nil), files.Nameservers...)}
	f.record("configure jail %s (%d rc.conf lines, key %t)", containerID, len(files.RCConf), files.AuthorizedKey != "")
	return nil
}

// JailFiles returns what ConfigureJail last wrote for a container.
func (f *Fake) JailFiles(containerID string) (hostops.JailFiles, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	files, ok := f.jailFiles[containerID]
	return files, ok
}

func (j fakeJails) TemplateHasSSHD(_ context.Context, _ string, templateID string) (bool, error) {
	f := j.f
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sshdTemplates[templateID], nil
}

// SetTemplateSSHD makes a simulated template have sshd, or not.
func (f *Fake) SetTemplateSSHD(templateID string, has bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sshdTemplates[templateID] = has
}

func (j fakeJails) StartJail(ctx context.Context, spec hostops.JailSpec) (hostops.JailState, error) {
	root, err := j.JailRoot(ctx, spec.Pool, spec.ContainerID)
	if err != nil {
		return hostops.JailState{}, err
	}
	logPath, err := j.f.conf.JailRunPath(spec.ContainerID, hostops.JailConsoleLog)
	if err != nil {
		return hostops.JailState{}, err
	}
	if _, err := hostops.RenderJailConf(spec, root, logPath); err != nil {
		return hostops.JailState{}, err
	}
	name, _ := hostops.JailName(spec.ContainerID)
	for _, nic := range spec.NICs {
		b, err := j.f.Bridge(ctx, nic.Bridge)
		if err != nil {
			return hostops.JailState{}, err
		}
		if err := hostops.CheckBridgeUsable(j.f.conf, b); err != nil {
			return hostops.JailState{}, err
		}
	}
	f := j.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.jails[spec.ContainerID]; ok {
		return hostops.JailState{}, &models.Error{Code: models.CodeContainerNotStopped, Message: "the jail is running"}
	}
	dhcp := false
	for _, nic := range spec.NICs {
		dhcp = dhcp || nic.DHCP
	}
	if spec.Limits.Any() && f.RacctDisabled {
		return hostops.JailState{}, &models.Error{Code: models.CodeRctlUnavailable, Message: "RACCT is disabled (simulated)"}
	}
	if len(spec.NICs) > 0 && f.NoEpair {
		return hostops.JailState{}, &models.Error{Code: models.CodeEpairUnavailable, Message: "the if_epair module is not loaded (simulated)"}
	}
	if dhcp {
		if f.DevfsConflict {
			return hostops.JailState{}, &models.Error{Code: models.CodeDevfsRulesetConflict,
				Message: "devfs ruleset 6001 holds rules Skep did not put there (simulated)"}
		}
		if !f.devfsDHCP {
			f.devfsDHCP = true
			f.record("devfs rule -s %d add (include 1-4, pf, bpf* unhide)", hostops.JailDHCPDevfsRuleset)
		}
	}
	f.destroyEpairsLocked(spec.ContainerID)
	epairs := map[int]string{}
	for _, nic := range spec.NICs {
		host := fmt.Sprintf("epair%da", f.nextEpair)
		f.nextEpair++
		f.epairs[host] = fakeTap{desc: hostops.InterfaceDescription(spec.ContainerID, nic.Slot), bridge: nic.Bridge}
		epairs[nic.Slot] = host
		f.record("ifconfig epair create description %s; %s addm %s", f.epairs[host].desc, nic.Bridge, host)
	}
	jid := f.nextJID
	f.nextJID++
	f.jails[spec.ContainerID] = jid
	rules, err := hostops.JailRctlRules(spec.ContainerID, spec.Limits)
	if err != nil {
		return hostops.JailState{}, err
	}
	delete(f.rctl, spec.ContainerID)
	if len(rules) > 0 {
		f.rctl[spec.ContainerID] = rules
		f.record("rctl -a %s", strings.Join(rules, " "))
	}
	f.record("jail -c %s; /dev checked; jexec %s /bin/sh /etc/rc", name, name)
	return hostops.JailState{JID: jid, Name: name, Path: root, Epairs: epairs}, nil
}

// DiscardJail removes a simulated jail without exec.stop, then its
// epairs.
func (j fakeJails) DiscardJail(_ context.Context, spec hostops.JailSpec) error {
	name, err := hostops.JailName(spec.ContainerID)
	if err != nil {
		return err
	}
	f := j.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.jails[spec.ContainerID]; ok {
		delete(f.jails, spec.ContainerID)
		f.record("jail -r %s (no exec.stop)", name)
	}
	f.destroyEpairsLocked(spec.ContainerID)
	delete(f.rctl, spec.ContainerID)
	return nil
}

// destroyEpairsLocked destroys a container's simulated epairs. f.mu is
// held.
func (f *Fake) destroyEpairsLocked(containerID string) []string {
	var out []string
	for name, e := range f.epairs {
		if strings.HasPrefix(e.desc, "skep:"+containerID+":") {
			delete(f.epairs, name)
			f.record("ifconfig %s destroy", name)
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func (j fakeJails) EpairAvailable(context.Context) error {
	j.f.mu.Lock()
	defer j.f.mu.Unlock()
	if j.f.NoEpair {
		return &models.Error{Code: models.CodeEpairUnavailable, Message: "the if_epair module is not loaded (simulated)"}
	}
	return nil
}

func (j fakeJails) DestroyJailInterfaces(_ context.Context, containerID string) ([]string, error) {
	if !hostops.ValidID(containerID) {
		return nil, models.Invalid("invalid container ID")
	}
	j.f.mu.Lock()
	defer j.f.mu.Unlock()
	return j.f.destroyEpairsLocked(containerID), nil
}

func (j fakeJails) SkepInterfaces(context.Context) ([]hostops.SkepInterface, error) {
	f := j.f
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []hostops.SkepInterface
	add := func(kind, name, desc string) {
		rest, ok := strings.CutPrefix(desc, "skep:")
		if !ok {
			return
		}
		id, slot, ok := strings.Cut(rest, ":")
		n, err := strconv.Atoi(slot)
		if ok && err == nil && hostops.ValidID(id) {
			out = append(out, hostops.SkepInterface{Name: name, Kind: kind, OwnerID: id, Slot: n})
		}
	}
	for name, e := range f.epairs {
		add("epair", name, e.desc)
	}
	for name, t := range f.taps {
		add("tap", name, t.desc)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// LeaveEpair simulates an epair a crashed agent left for a container.
func (f *Fake) LeaveEpair(containerID string, slot int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	host := fmt.Sprintf("epair%da", f.nextEpair)
	f.nextEpair++
	f.epairs[host] = fakeTap{desc: hostops.InterfaceDescription(containerID, slot), bridge: "bridge0"}
	return host
}

// Epairs returns the simulated epair host ends and their descriptions.
func (f *Fake) Epairs() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for name, e := range f.epairs {
		out[name] = e.desc
	}
	return out
}

func (j fakeJails) StopJail(_ context.Context, spec hostops.JailSpec) error {
	name, err := hostops.JailName(spec.ContainerID)
	if err != nil {
		return err
	}
	f := j.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.jails[spec.ContainerID]; ok {
		delete(f.jails, spec.ContainerID)
		f.record("jail -r %s", name)
	}
	f.destroyEpairsLocked(spec.ContainerID)
	delete(f.rctl, spec.ContainerID)
	return nil
}

func (j fakeJails) RunningJails(context.Context) (map[string]hostops.JailState, error) {
	f := j.f
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]hostops.JailState{}
	for id, jid := range f.jails {
		name, _ := hostops.JailName(id)
		out[id] = hostops.JailState{JID: jid, Name: name}
	}
	return out, nil
}

// SetRunningJailForTest makes a container's simulated jail run without a
// start, and returns its JID.
func (f *Fake) SetRunningJailForTest(containerID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	jid := f.nextJID
	f.nextJID++
	f.jails[containerID] = jid
	return jid
}

// SetJailStopped removes a simulated jail as if it had been removed
// outside Skep.
func (f *Fake) SetJailStopped(containerID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.jails, containerID)
}

func (j fakeJails) ConsoleLogTail(containerID string, _ int) (string, error) {
	if !hostops.ValidID(containerID) {
		return "", models.Invalid("invalid container ID")
	}
	f := j.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.jails[containerID]; ok {
		return "Starting syslogd.\nStarting cron.\n(simulated)\n", nil
	}
	return "", nil
}

func (j fakeJails) RemoveJailRun(containerID string) error {
	if !hostops.ValidID(containerID) {
		return models.Invalid("invalid container ID")
	}
	return nil
}

// RctlInfo implements hostops.Jails.
func (j fakeJails) RctlInfo(context.Context) (hostops.RctlInfo, error) {
	f := j.f
	f.mu.Lock()
	defer f.mu.Unlock()
	cpus := f.CPUs
	if cpus == 0 {
		cpus = 4
	}
	return hostops.RctlInfo{Enabled: !f.RacctDisabled, CPUs: cpus}, nil
}

// JailUsage implements hostops.Jails: JailUsages[id] for a running jail
// (zeros when unset).
func (j fakeJails) JailUsage(_ context.Context, containerID string) (hostops.JailUsage, error) {
	f := j.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.RacctDisabled {
		return hostops.JailUsage{}, &models.Error{Code: models.CodeRctlUnavailable, Message: "RACCT is disabled (simulated)"}
	}
	if _, ok := f.jails[containerID]; !ok {
		return hostops.JailUsage{}, fmt.Errorf("jail of %s: %w", containerID, models.ErrNotFound)
	}
	f.record("rctl -u jail:skep-%s", containerID)
	return f.JailUsages[containerID], nil
}

// RctlRules returns the simulated RCTL rules on a container's jail.
func (f *Fake) RctlRules(containerID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rctl[containerID]...)
}

// SetJailQuota implements hostops.Jails.
func (j fakeJails) SetJailQuota(_ context.Context, pool, containerID string, bytes int64) error {
	if bytes < 0 {
		return models.Invalid("a disk quota cannot be negative")
	}
	ds, err := j.f.conf.JailDataset(pool, containerID)
	if err != nil {
		return err
	}
	f := j.f
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.datasets[ds]
	if !ok || d.ContainerID != containerID {
		return fmt.Errorf("the jail dataset %s: %w", ds, models.ErrNotFound)
	}
	if bytes > 0 && bytes < d.ReferencedBytes {
		return models.Invalid("the disk quota is below what the jail's root already uses")
	}
	d.RefQuotaBytes = bytes
	f.datasets[ds] = d
	f.record("zfs set refquota=%d %s", bytes, ds)
	return nil
}
