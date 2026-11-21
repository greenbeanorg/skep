package hostops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/greenbeanorg/skep/pkg/models"
)

// Jail templates (ADR 0010 decision 7 and the step 6 decision on package
// scripts). This file holds the dataset and directory side; the pkg,
// build jail, and host-file sides are in templates_pkg.go,
// templates_jail.go, and hostfiles.go. The order of a build, and every
// decision about what is acceptable, belongs to the template service;
// hostops only offers typed steps whose names and paths are derived from
// the pool and the template ID.
//
// Layout, in an allow-listed ZFS pool:
//
//	<pool>/<root>/templates              mounted, its directory root:wheel 0700
//	<pool>/<root>/templates/<id>         the template root, skep:template_id=<id>
//	<templates mountpoint>/.work-<id>/   the work area: tmp/ and cache/
//	<pool>/<root>/templates/<id>@skep-base
//
// The work area is a plain directory beside the template root under the
// same 0700 parent (the second form item 8 of the decision allows), so it
// is never part of the template's dataset or snapshot.

// Templates builds and destroys jail templates.
type Templates interface {
	// HostRelease is freebsd-version -k of the host.
	HostRelease(ctx context.Context) (string, error)
	// PkgbaseAvailable says whether the host has the pkg the builder runs
	// (/usr/local/sbin/pkg, which the /usr/sbin/pkg bootstrap execs), and
	// if not, why.
	PkgbaseAvailable() (bool, string)
	// DistAvailable says whether base.txz and MANIFEST are in the
	// configured dist directory, and if not, why.
	DistAvailable() (bool, string)

	// EnsureTemplatesParent creates <pool>/<root>/templates on first use,
	// with its directory root:wheel 0700, and then checks it as
	// CheckTemplatesParent does.
	EnsureTemplatesParent(ctx context.Context, pool string) error
	// CheckTemplatesParent refuses (jail_parent_insecure) a templates
	// directory that is not a real directory owned by root:wheel with
	// mode 0700, or that does not exist.
	CheckTemplatesParent(ctx context.Context, pool string) error
	// PrepareTemplate checks the templates directory, creates the
	// template's dataset and its empty work area, and returns the paths.
	PrepareTemplate(ctx context.Context, pool, templateID string) (TemplatePaths, error)
	// TemplatePaths returns the paths of an existing template's dataset.
	TemplatePaths(ctx context.Context, pool, templateID string) (TemplatePaths, error)
	// RemoveWorkArea removes a template's work area. A missing one is not
	// an error.
	RemoveWorkArea(ctx context.Context, pool, templateID string) error
	// FinishTemplate takes @skep-base, carrying skep:template_id, sets
	// the template read-only, and returns its dataset.
	FinishTemplate(ctx context.Context, pool, templateID string) (Dataset, error)
	// DestroyTemplate destroys a template's dataset and its snapshots, and
	// removes its work area. It refuses a dataset that does not carry the
	// template's ID, and a template whose snapshot has clones
	// (snapshot_has_clones). A template without a dataset yields nil.
	DestroyTemplate(ctx context.Context, pool, templateID string) ([]Dataset, error)

	// PkgInstall installs packages into the template root with the fixed
	// environment and options of the decision: package scripts and
	// triggers off, the cache and TMPDIR in the work area, the database
	// in the root. Names must match ^FreeBSD-[a-z0-9][a-z0-9-]*$.
	PkgInstall(ctx context.Context, pool, templateID string, packages []string) error
	// PkgScripts reads every installed package's scripts from the
	// template's own database (pkg info --raw).
	PkgScripts(ctx context.Context, pool, templateID string) ([]PackageScripts, error)
	// TriggerFiles reads every entry of the template's trigger
	// directories. A non-regular entry is returned with Regular false.
	TriggerFiles(ctx context.Context, pool, templateID string) ([]TriggerFile, error)
	// TemplateRelease reads USERLAND_VERSION from the template's
	// bin/freebsd-version without running it.
	TemplateRelease(ctx context.Context, pool, templateID string) (string, error)
	// SSHHostKeys lists the template's etc/ssh/ssh_host_* entries; a
	// template must have none (amendment g).
	SSHHostKeys(ctx context.Context, pool, templateID string) ([]string, error)
	// DeferredTriggers lists the files in the template's
	// var/db/pkg/triggers.
	DeferredTriggers(ctx context.Context, pool, templateID string) ([]string, error)
	// RemoveCatalogues removes the template's var/db/pkg/repos.
	RemoveCatalogues(ctx context.Context, pool, templateID string) error
	// CacheEntries lists what is in the template's var/cache/pkg.
	CacheEntries(ctx context.Context, pool, templateID string) ([]string, error)
	// ExtractDist checks base.txz of the dist directory against its
	// MANIFEST and extracts it into the template root. It returns the
	// archive's SHA-256. A mismatch is template_archive_mismatch.
	ExtractDist(ctx context.Context, pool, templateID string) (string, error)

	// StartBuildJail creates the build jail skep-build-<id> on the
	// template root with the fixed parameters of the decision, and
	// checks them as the kernel reports them.
	StartBuildJail(ctx context.Context, pool, templateID string) error
	// BuildJailExec runs one of the fixed build commands in the build
	// jail with jexec -l. ran is false when the command does not apply to
	// this template (its target directory is absent).
	BuildJailExec(ctx context.Context, pool, templateID string, cmd BuildCommand) (ran bool, err error)
	// RemoveBuildJail removes the build jail and confirms that it is
	// gone. A jail that does not exist is not an error.
	RemoveBuildJail(ctx context.Context, templateID string) error
	// BuildJails lists the template IDs of every skep-build-* jail.
	BuildJails(ctx context.Context) ([]string, error)

	// HostFiles records the state of every regular file and symbolic link
	// under the host's /etc, /var/db/pkg, and /usr/share/certs.
	HostFiles(ctx context.Context) (HostFileState, error)
}

// TemplatePaths are the host paths of a template.
type TemplatePaths struct {
	Dataset string
	// Root is the template root (the dataset's mountpoint).
	Root string
	// Work is the work area; Tmp and Cache are under it.
	Work  string
	Tmp   string
	Cache string
}

// PackageScripts are the scripts of one installed package, as pkg info
// --raw gives them.
type PackageScripts struct {
	Name    string
	Version string
	// Scripts maps a phase (post-install, pre-deinstall, ...) to the
	// shell script body.
	Scripts map[string]string
	// LuaScripts maps a phase to its Lua script bodies.
	LuaScripts map[string][]string
}

// TriggerFile is one entry of a template's trigger directory.
type TriggerFile struct {
	// Path is relative to the template root, for example
	// usr/share/pkg/triggers/mandoc.ucl.
	Path string
	// Regular is false for a symbolic link, a directory, or anything else
	// that is not a regular file; Content is then empty.
	Regular bool
	Content []byte
}

// The template layout.
const (
	templatesDir    = "templates"
	workPrefix      = ".work-"
	baseSnapshot    = "skep-base"
	BaseSnapshot    = baseSnapshot
	BuildJailPrefix = "skep-build-"
)

// TriggerDirs are the template's trigger directories, relative to its
// root: the base system's and pkg's default PKG_TRIGGERS_DIR.
var TriggerDirs = []string{"usr/share/pkg/triggers", "usr/local/share/pkg/triggers"}

// TemplatesDataset is <pool>/<root>/templates.
func (c Confinement) TemplatesDataset(pool string) (string, error) {
	root, err := c.rootDataset(pool)
	if err != nil {
		return "", err
	}
	return root + "/" + templatesDir, nil
}

// TemplateDataset is <pool>/<root>/templates/<template-id>.
func (c Confinement) TemplateDataset(pool, templateID string) (string, error) {
	parent, err := c.TemplatesDataset(pool)
	if err != nil {
		return "", err
	}
	if !ValidID(templateID) {
		return "", invalid("invalid template ID")
	}
	return parent + "/" + templateID, nil
}

// BuildJailName is the name of a template's build jail.
func BuildJailName(templateID string) (string, error) {
	if !ValidID(templateID) {
		return "", invalid("invalid template ID")
	}
	return BuildJailPrefix + templateID, nil
}

func jailParentInsecure(dir, why string) error {
	return &models.Error{
		Code:    models.CodeJailParentInsecure,
		Message: "the directory " + dir + " " + why + "; it must be a directory owned by root:wheel with mode 0700, because it holds jail and template roots",
		Details: map[string]any{"path": dir},
	}
}

// checkPrivateDir refuses a directory that is a symbolic link, not a
// directory, or not root:wheel 0700 (ADR 0010 decision 9).
func checkPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return jailParentInsecure(dir, "does not exist")
	}
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return jailParentInsecure(dir, "is not a directory")
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("hostops: cannot read the owner of %s", dir)
	}
	if st.Uid != 0 || st.Gid != 0 {
		return jailParentInsecure(dir, fmt.Sprintf("is owned by %d:%d", st.Uid, st.Gid))
	}
	if perm := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); perm != 0o700 {
		return jailParentInsecure(dir, fmt.Sprintf("has mode %04o", perm))
	}
	return nil
}

// mountInfo reads whether a filesystem is mounted and where.
func (h *host) mountInfo(ctx context.Context, name string) (mounted bool, mountpoint string, err error) {
	out, err := h.run.Run(ctx, zfsBinary, "get", "-H", "-p", "-o", "value", "mounted,mountpoint", name)
	if err != nil {
		return false, "", err
	}
	f := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(f) != 2 {
		return false, "", fmt.Errorf("zfs get mounted,mountpoint: unexpected output %q", firstLine(out))
	}
	mp := strings.TrimSpace(f[1])
	return strings.TrimSpace(f[0]) == "yes", mp, nil
}

// templatesMount returns the mountpoint of <root>/templates, which must be
// mounted at an absolute, clean path.
func (h *host) templatesMount(ctx context.Context, pool string) (string, error) {
	name, err := h.conf.TemplatesDataset(pool)
	if err != nil {
		return "", err
	}
	return h.privateMount(ctx, name)
}

// privateMount returns the mountpoint of one of the 0700 parent datasets
// (<root>/templates, <root>/jail), which must be mounted at an absolute,
// clean path. A dataset that does not exist is jail_parent_insecure.
func (h *host) privateMount(ctx context.Context, name string) (string, error) {
	mounted, mp, err := h.mountInfo(ctx, name)
	if err != nil {
		if stderrContains(err, "does not exist") {
			return "", jailParentInsecure(name, "does not exist")
		}
		return "", err
	}
	if !mounted || !filepath.IsAbs(mp) || filepath.Clean(mp) != mp || mp == "/" {
		return "", storageUnavailable("the dataset " + name + " is not mounted at an absolute path; jails and templates need it mounted")
	}
	return mp, nil
}

// CheckTemplatesParent implements Templates.
func (h *host) CheckTemplatesParent(ctx context.Context, pool string) error {
	name, err := h.conf.TemplatesDataset(pool)
	if err != nil {
		return err
	}
	return h.checkPrivateParent(ctx, name)
}

// EnsureTemplatesParent implements Templates. It creates <pool>/<root>
// and <root>/templates on first use. Unlike the VM datasets, templates is mounted: it holds the
// work areas, and its directory is the 0700 parent of every template.
// The mode is set only when Skep creates it; afterwards it is checked.
func (h *host) EnsureTemplatesParent(ctx context.Context, pool string) error {
	name, err := h.conf.TemplatesDataset(pool)
	if err != nil {
		return err
	}
	return h.ensurePrivateParent(ctx, pool, name)
}

// ensurePrivateParent creates <pool>/<root> and one of the 0700 parent
// datasets under it on first use, mounted, with its directory set to
// root:wheel 0700, and then checks it. An existing one is only checked.
func (h *host) ensurePrivateParent(ctx context.Context, pool, name string) error {
	if err := h.zfsLoaded(); err != nil {
		return err
	}
	parent := pool
	for _, part := range strings.Split(h.conf.DatasetRoot, "/") {
		parent += "/" + part
		if err := h.ensureUnlockedDataset(ctx, parent); err != nil {
			return err
		}
	}
	existing, err := h.listDatasets(ctx, name, false)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		if err := checkUnlocked(existing); err != nil {
			return err
		}
		return h.checkPrivateParent(ctx, name)
	}
	if _, err := h.run.Run(ctx, zfsBinary, "create", "-o", "canmount=on", name); err != nil {
		return err
	}
	mp, err := h.privateMount(ctx, name)
	if err != nil {
		return err
	}
	if err := os.Lchown(mp, 0, 0); err != nil {
		return err
	}
	if err := os.Chmod(mp, 0o700); err != nil {
		return err
	}
	return h.checkPrivateParent(ctx, name)
}

// checkPrivateParent checks the directory of a 0700 parent dataset.
func (h *host) checkPrivateParent(ctx context.Context, name string) error {
	if err := h.zfsLoaded(); err != nil {
		return err
	}
	mp, err := h.privateMount(ctx, name)
	if err != nil {
		return err
	}
	return checkPrivateDir(mp)
}

// templatePaths derives the paths of a template from the templates
// mountpoint, and checks that its dataset is mounted where it should be.
func (h *host) templatePaths(ctx context.Context, pool, templateID string) (TemplatePaths, error) {
	ds, err := h.conf.TemplateDataset(pool, templateID)
	if err != nil {
		return TemplatePaths{}, err
	}
	mp, err := h.templatesMount(ctx, pool)
	if err != nil {
		return TemplatePaths{}, err
	}
	work := filepath.Join(mp, workPrefix+templateID)
	p := TemplatePaths{Dataset: ds, Root: filepath.Join(mp, templateID), Work: work,
		Tmp: filepath.Join(work, "tmp"), Cache: filepath.Join(work, "cache")}
	return p, nil
}

// checkTemplateMounted refuses a template dataset that is not mounted at
// <templates mountpoint>/<id>, so no path below ever lands elsewhere.
func (h *host) checkTemplateMounted(ctx context.Context, p TemplatePaths) error {
	mounted, mp, err := h.mountInfo(ctx, p.Dataset)
	if err != nil {
		return err
	}
	if !mounted || mp != p.Root {
		return storageUnavailable("the template dataset " + p.Dataset + " is not mounted at " + p.Root)
	}
	return nil
}

// PrepareTemplate implements Templates.
func (h *host) PrepareTemplate(ctx context.Context, pool, templateID string) (TemplatePaths, error) {
	ds, err := h.conf.TemplateDataset(pool, templateID)
	if err != nil {
		return TemplatePaths{}, err
	}
	if err := h.zfsLoaded(); err != nil {
		return TemplatePaths{}, err
	}
	if err := h.CheckTemplatesParent(ctx, pool); err != nil {
		return TemplatePaths{}, err
	}
	p, err := h.templatePaths(ctx, pool, templateID)
	if err != nil {
		return TemplatePaths{}, err
	}
	if existing, err := h.listDatasets(ctx, ds, false); err != nil {
		return TemplatePaths{}, err
	} else if len(existing) > 0 {
		return TemplatePaths{}, fmt.Errorf("hostops: the template dataset %s already exists", ds)
	}
	// A template root must be empty when pkg starts: pkg looks for trust
	// anchors inside the root before the host's (the step 6 trace).
	if _, err := h.run.Run(ctx, zfsBinary, "create", "-o", "canmount=on", "-o", propTemplateID+"="+templateID, ds); err != nil {
		return TemplatePaths{}, err
	}
	if err := h.checkTemplateMounted(ctx, p); err != nil {
		return TemplatePaths{}, err
	}
	if err := emptyDir(p.Root); err != nil {
		return TemplatePaths{}, err
	}
	if err := os.Mkdir(p.Work, 0o700); err != nil {
		return TemplatePaths{}, fmt.Errorf("hostops: create the work area: %w", err)
	}
	for _, d := range []string{p.Tmp, p.Cache} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return TemplatePaths{}, fmt.Errorf("hostops: create the work area: %w", err)
		}
	}
	return p, nil
}

// emptyDir refuses a directory that holds anything. A new ZFS file
// system is empty.
func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("hostops: the new template root %s is not empty", dir)
	}
	return nil
}

// TemplatePaths implements Templates.
func (h *host) TemplatePaths(ctx context.Context, pool, templateID string) (TemplatePaths, error) {
	if err := h.zfsLoaded(); err != nil {
		return TemplatePaths{}, err
	}
	p, err := h.templatePaths(ctx, pool, templateID)
	if err != nil {
		return TemplatePaths{}, err
	}
	if err := h.checkTemplateMounted(ctx, p); err != nil {
		return TemplatePaths{}, err
	}
	return p, nil
}

// RemoveWorkArea implements Templates. The work area is under the 0700
// templates directory and was written only by pkg, as root.
func (h *host) RemoveWorkArea(ctx context.Context, pool, templateID string) error {
	if err := h.zfsLoaded(); err != nil {
		return err
	}
	mp, err := h.templatesMount(ctx, pool)
	if err != nil {
		return err
	}
	if !ValidID(templateID) {
		return invalid("invalid template ID")
	}
	r, err := os.OpenRoot(mp)
	if err != nil {
		return err
	}
	defer r.Close()
	return r.RemoveAll(workPrefix + templateID)
}

// FinishTemplate implements Templates.
func (h *host) FinishTemplate(ctx context.Context, pool, templateID string) (Dataset, error) {
	ds, err := h.conf.TemplateDataset(pool, templateID)
	if err != nil {
		return Dataset{}, err
	}
	if err := h.zfsLoaded(); err != nil {
		return Dataset{}, err
	}
	d, err := h.ownedTemplate(ctx, ds, templateID)
	if err != nil {
		return Dataset{}, err
	}
	if d == nil {
		return Dataset{}, notFound("template dataset " + ds)
	}
	if _, err := h.run.Run(ctx, zfsBinary, "snapshot", "-o", propTemplateID+"="+templateID, ds+"@"+baseSnapshot); err != nil {
		return Dataset{}, err
	}
	if _, err := h.run.Run(ctx, zfsBinary, "set", "readonly=on", ds); err != nil {
		return Dataset{}, err
	}
	return h.oneDataset(ctx, ds)
}

// ownedTemplate returns a template's dataset, nil if it does not exist,
// and refuses one that does not carry the template's ID.
func (h *host) ownedTemplate(ctx context.Context, ds, templateID string) (*Dataset, error) {
	list, err := h.listDatasets(ctx, ds, true)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	for _, d := range list {
		if d.Name != ds {
			return nil, fmt.Errorf("hostops: refusing %s: a template has no child datasets", d.Name)
		}
		if d.TemplateID != templateID {
			return nil, fmt.Errorf("hostops: refusing %s: its %s property does not name this template", d.Name, propTemplateID)
		}
	}
	return &list[0], nil
}

// DestroyTemplate implements Templates. It never uses -R.
func (h *host) DestroyTemplate(ctx context.Context, pool, templateID string) ([]Dataset, error) {
	ds, err := h.conf.TemplateDataset(pool, templateID)
	if err != nil {
		return nil, err
	}
	if err := h.zfsLoaded(); err != nil {
		return nil, err
	}
	d, err := h.ownedTemplate(ctx, ds, templateID)
	if err != nil {
		return nil, err
	}
	var destroyed []Dataset
	if d != nil {
		snaps, err := h.listSnapshots(ctx, ds, false)
		if err != nil {
			return nil, err
		}
		if clones := clonesOf(snaps); len(clones) > 0 {
			return nil, hasClones(clones)
		}
		if _, err := h.run.Run(ctx, zfsBinary, "destroy", "-r", ds); err != nil {
			return nil, err
		}
		destroyed = append([]Dataset{*d}, snaps...)
	}
	if _, err := h.templatesMount(ctx, pool); err == nil {
		if err := h.RemoveWorkArea(ctx, pool, templateID); err != nil {
			return destroyed, err
		}
	}
	return destroyed, nil
}

// openTemplateRoot opens an existing template's root as an os.Root, so
// no name below can resolve outside it.
func (h *host) openTemplateRoot(ctx context.Context, pool, templateID string) (*os.Root, TemplatePaths, error) {
	p, err := h.TemplatePaths(ctx, pool, templateID)
	if err != nil {
		return nil, TemplatePaths{}, err
	}
	r, err := os.OpenRoot(p.Root)
	if err != nil {
		return nil, TemplatePaths{}, err
	}
	return r, p, nil
}

// lstatDirChain checks that every component of rel (slash-separated,
// relative) is a real directory, not a symbolic link. exists is false
// when a component is missing.
func lstatDirChain(r *os.Root, rel string) (exists bool, err error) {
	cur := ""
	for _, part := range strings.Split(rel, "/") {
		cur = path.Join(cur, part)
		fi, err := r.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 {
			return false, fmt.Errorf("hostops: %s in the template is not a directory", cur)
		}
	}
	return true, nil
}

// listDir lists the entry names of a directory of the template that must
// be a real directory; a missing one yields nil.
func listDir(r *os.Root, rel string) ([]fs.DirEntry, error) {
	exists, err := lstatDirChain(r, rel)
	if err != nil || !exists {
		return nil, err
	}
	f, err := r.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

// TriggerFiles implements Templates.
func (h *host) TriggerFiles(ctx context.Context, pool, templateID string) ([]TriggerFile, error) {
	r, _, err := h.openTemplateRoot(ctx, pool, templateID)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []TriggerFile
	for _, dir := range TriggerDirs {
		entries, err := listDir(r, dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			rel := dir + "/" + e.Name()
			tf := TriggerFile{Path: rel}
			if e.Type().IsRegular() {
				if tf.Content, err = readRegular(r, rel, maxTriggerFile); err != nil {
					return nil, err
				}
				tf.Regular = true
			}
			out = append(out, tf)
		}
	}
	return out, nil
}

// maxTriggerFile bounds what is read of one trigger file.
const maxTriggerFile = 1 << 20

// readRegular reads a regular file of the template without following a
// symbolic link in its last component: it is checked with Lstat, opened,
// and checked again to be the same file.
func readRegular(r *os.Root, rel string, limit int64) ([]byte, error) {
	before, err := r.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("hostops: %s in the template is not a regular file", rel)
	}
	f, err := r.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) {
		return nil, fmt.Errorf("hostops: %s in the template changed while it was read", rel)
	}
	if after.Size() > limit {
		return nil, fmt.Errorf("hostops: %s in the template is larger than %d bytes", rel, limit)
	}
	buf := make([]byte, after.Size())
	n, err := readFull(f, buf)
	return buf[:n], err
}

func readFull(f *os.File, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := f.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// DeferredTriggers implements Templates.
func (h *host) DeferredTriggers(ctx context.Context, pool, templateID string) ([]string, error) {
	return h.entries(ctx, pool, templateID, "var/db/pkg/triggers")
}

// SSHHostKeys implements Templates.
func (h *host) SSHHostKeys(ctx context.Context, pool, templateID string) ([]string, error) {
	all, err := h.entries(ctx, pool, templateID, "etc/ssh")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range all {
		if strings.HasPrefix(path.Base(e), "ssh_host_") {
			out = append(out, e)
		}
	}
	return out, nil
}

// CacheEntries implements Templates.
func (h *host) CacheEntries(ctx context.Context, pool, templateID string) ([]string, error) {
	return h.entries(ctx, pool, templateID, "var/cache/pkg")
}

func (h *host) entries(ctx context.Context, pool, templateID, rel string) ([]string, error) {
	r, _, err := h.openTemplateRoot(ctx, pool, templateID)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	list, err := listDir(r, rel)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range list {
		out = append(out, rel+"/"+e.Name())
	}
	return out, nil
}

// RemoveCatalogues implements Templates.
func (h *host) RemoveCatalogues(ctx context.Context, pool, templateID string) error {
	r, _, err := h.openTemplateRoot(ctx, pool, templateID)
	if err != nil {
		return err
	}
	defer r.Close()
	exists, err := lstatDirChain(r, "var/db/pkg")
	if err != nil || !exists {
		return err
	}
	return r.RemoveAll("var/db/pkg/repos")
}
