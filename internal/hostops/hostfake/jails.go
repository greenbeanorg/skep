package hostfake

import (
	"context"
	"fmt"
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
	f.jailFiles[containerID] = hostops.JailFiles{RCConf: append([]string(nil), files.RCConf...), AuthorizedKey: files.AuthorizedKey}
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
	f := j.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.jails[spec.ContainerID]; ok {
		return hostops.JailState{}, &models.Error{Code: models.CodeContainerNotStopped, Message: "the jail is running"}
	}
	jid := f.nextJID
	f.nextJID++
	f.jails[spec.ContainerID] = jid
	f.record("jail -c %s", name)
	return hostops.JailState{JID: jid, Name: name, Path: root}, nil
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
