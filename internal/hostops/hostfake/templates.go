package hostfake

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// SimulatedRelease is the release the fake's host and templates report.
const SimulatedRelease = "15.1-RELEASE-p6"

// fakeTemplates simulates the template builder: the datasets appear in
// the fake's pools, the packages and commands are recorded, and nothing
// is installed or run. Its package scripts are the 15.1 minimal set's, so
// a simulated build passes the real allowlist.
type fakeTemplates struct{ f *Fake }

// simulatedScripts are the three scripted packages of the 15.1 minimal
// set, with their bodies exactly as pkg info --raw gave them on hearst.
var simulatedScripts = []hostops.PackageScripts{
	{Name: "FreeBSD-caroot", Version: "15.1p6", Scripts: map[string]string{
		"post-install":   "/usr/sbin/certctl -D${PKG_ROOTDIR}/ ${PKG_METALOG:+-U -M $PKG_METALOG} rehash",
		"post-deinstall": "/usr/sbin/certctl -D${PKG_ROOTDIR}/ ${PKG_METALOG:+-U -M $PKG_METALOG} rehash",
	}},
	{Name: "FreeBSD-runtime", Version: "15.1p6", Scripts: map[string]string{
		"post-install": "pwd_mkdb -i -p -d  ${PKG_ROOTDIR}/etc ${PKG_ROOTDIR}/etc/master.passwd\n    services_mkdb -l -q -o ${PKG_ROOTDIR}/var/db/services.db ${PKG_ROOTDIR}/",
	}},
	{Name: "FreeBSD-utilities", Version: "15.1p4", Scripts: map[string]string{
		"post-install": "cap_mkdb -l ${PKG_ROOTDIR}/etc/login.conf",
	}},
}

func (t fakeTemplates) paths(pool, id string) (hostops.TemplatePaths, error) {
	ds, err := t.f.conf.TemplateDataset(pool, id)
	if err != nil {
		return hostops.TemplatePaths{}, err
	}
	root := "/simulated/" + ds
	work := "/simulated/" + strings.TrimSuffix(ds, id) + ".work-" + id
	return hostops.TemplatePaths{Dataset: ds, Root: root, Work: work, Tmp: work + "/tmp", Cache: work + "/cache"}, nil
}

func (t fakeTemplates) HostRelease(context.Context) (string, error) { return SimulatedRelease, nil }
func (t fakeTemplates) PkgbaseAvailable() (bool, string)            { return true, "" }
func (t fakeTemplates) DistAvailable() (bool, string) {
	return false, "the simulated host has no " + hostops.DistArchive
}

func (t fakeTemplates) EnsureTemplatesParent(_ context.Context, pool string) error {
	name, err := t.f.conf.TemplatesDataset(pool)
	if err != nil {
		return err
	}
	f := t.f
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

func (t fakeTemplates) CheckTemplatesParent(ctx context.Context, pool string) error {
	return t.EnsureTemplatesParent(ctx, pool)
}

func (t fakeTemplates) PrepareTemplate(_ context.Context, pool, id string) (hostops.TemplatePaths, error) {
	p, err := t.paths(pool, id)
	if err != nil {
		return p, err
	}
	f := t.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.datasets[p.Dataset]; ok {
		return p, fmt.Errorf("hostfake: the template dataset %s already exists", p.Dataset)
	}
	f.txg++
	f.datasets[p.Dataset] = hostops.Dataset{Name: p.Dataset, Type: "filesystem", TemplateID: id, CreateTxg: f.txg}
	f.record("zfs create %s", p.Dataset)
	return p, nil
}

func (t fakeTemplates) TemplatePaths(_ context.Context, pool, id string) (hostops.TemplatePaths, error) {
	return t.paths(pool, id)
}

func (t fakeTemplates) RemoveWorkArea(_ context.Context, pool, id string) error {
	p, err := t.paths(pool, id)
	if err == nil {
		t.f.locked(func() { t.f.record("rm -r %s", p.Work) })
	}
	return err
}

func (t fakeTemplates) FinishTemplate(_ context.Context, pool, id string) (hostops.Dataset, error) {
	p, err := t.paths(pool, id)
	if err != nil {
		return hostops.Dataset{}, err
	}
	f := t.f
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.datasets[p.Dataset]
	if !ok {
		return hostops.Dataset{}, fmt.Errorf("template dataset %s: %w", p.Dataset, models.ErrNotFound)
	}
	d.UsedBytes, d.ReferencedBytes = 132<<20, 132<<20
	f.datasets[p.Dataset] = d
	f.txg++
	snap := p.Dataset + "@" + hostops.BaseSnapshot
	f.snaps[snap] = hostops.Dataset{Name: snap, Type: "snapshot", TemplateID: id, CreateTxg: f.txg}
	f.record("zfs snapshot %s", snap)
	return d, nil
}

func (t fakeTemplates) DestroyTemplate(_ context.Context, pool, id string) ([]hostops.Dataset, error) {
	p, err := t.paths(pool, id)
	if err != nil {
		return nil, err
	}
	f := t.f
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []hostops.Dataset
	if d, ok := f.datasets[p.Dataset]; ok {
		if d.TemplateID != id {
			return nil, fmt.Errorf("hostfake: refusing %s: it does not name template %s", d.Name, id)
		}
		out = append(out, d)
		delete(f.datasets, p.Dataset)
		f.record("zfs destroy -r %s", p.Dataset)
	}
	for name, s := range f.snaps {
		if under(name, p.Dataset) {
			out = append(out, s)
			delete(f.snaps, name)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (t fakeTemplates) PkgInstall(_ context.Context, pool, id string, pkgs []string) error {
	for _, p := range pkgs {
		if !hostops.ValidTemplatePackage(p) {
			return models.Invalid("invalid template package name")
		}
	}
	t.f.locked(func() {
		t.f.record("pkg install %s into template %s", strings.Join(pkgs, " "), id)
		for _, p := range pkgs {
			if p == "FreeBSD-ssh" {
				t.f.sshdTemplates[id] = true
			}
		}
	})
	return nil
}

func (t fakeTemplates) PkgScripts(context.Context, string, string) ([]hostops.PackageScripts, error) {
	return append([]hostops.PackageScripts(nil), simulatedScripts...), nil
}

func (t fakeTemplates) TriggerFiles(context.Context, string, string) ([]hostops.TriggerFile, error) {
	return nil, nil
}

func (t fakeTemplates) TemplateRelease(context.Context, string, string) (string, error) {
	return SimulatedRelease, nil
}

func (t fakeTemplates) DeferredTriggers(context.Context, string, string) ([]string, error) {
	return nil, nil
}
func (t fakeTemplates) RemoveCatalogues(context.Context, string, string) error { return nil }
func (t fakeTemplates) SSHHostKeys(context.Context, string, string) ([]string, error) {
	return nil, nil
}
func (t fakeTemplates) CacheEntries(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (t fakeTemplates) ExtractDist(context.Context, string, string) (string, error) {
	return "", &models.Error{Code: models.CodeStorageFeatureUnsupported, Message: "the simulated host has no dist directory"}
}

func (t fakeTemplates) StartBuildJail(_ context.Context, _, id string) error {
	t.f.locked(func() { t.f.record("jail -c skep-build-%s", id) })
	return nil
}

func (t fakeTemplates) BuildJailExec(_ context.Context, _, id string, cmd hostops.BuildCommand) (bool, error) {
	t.f.locked(func() { t.f.record("jexec -l skep-build-%s %s", id, cmd) })
	return true, nil
}

func (t fakeTemplates) RemoveBuildJail(_ context.Context, id string) error {
	if !hostops.ValidID(id) {
		return models.Invalid("invalid template ID")
	}
	return nil
}

func (t fakeTemplates) BuildJails(context.Context) ([]string, error) { return nil, nil }

func (t fakeTemplates) HostFiles(context.Context) (hostops.HostFileState, error) {
	return hostops.HostFileState{}, nil
}

// locked runs fn with the fake's lock held.
func (f *Fake) locked(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}
