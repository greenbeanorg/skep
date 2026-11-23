package hostops

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/pkgrepo"
	"github.com/greenbeanorg/skep/pkg/models"
)

// The binaries of the template builder.
const (
	pkgBootstrap         = "/usr/sbin/pkg"
	pkgBinary            = "/usr/local/sbin/pkg"
	freebsdVersionBinary = "/bin/freebsd-version"
	tarBinary            = "/usr/bin/tar"
)

// Dist file names in storage.dist_dir.
const (
	DistArchive  = "base.txz"
	DistManifest = "MANIFEST"
)

// Timeouts and output limits of the long builder commands. The task's own
// 30-minute limit and cancellation bound them as well.
const (
	pkgInstallTimeout = 25 * time.Minute
	pkgInfoTimeout    = 2 * time.Minute
	tarTimeout        = 15 * time.Minute
	// pkg info --raw of the minimal set is about 1.8 MB (step 6 probe).
	pkgInfoMaxOutput = 64 << 20
)

// EnvCommand is a command run with an environment of its own instead of
// the Runner's fixed one.
type EnvCommand struct {
	Path string
	Args []string
	// Stdin, when not nil, is the command's whole standard input (ngctl
	// -f -, ADR 0011). Otherwise the command gets none.
	Stdin []byte
	// Env is the whole environment: nothing is inherited.
	Env     []string
	Timeout time.Duration
	// MaxOutput caps what is kept of standard output; 0 uses the
	// Runner's default. Output past it is an error unless MarkOverflow is
	// set.
	MaxOutput int
	// MarkOverflow keeps the first and the last MaxOutput bytes of
	// standard output, with a "skep: N bytes discarded" line between them,
	// instead of failing when the command writes more (ADR 0010
	// amendments o and p: jail -r, whose output is the jail's
	// /etc/rc.shutdown's and must never fail a removal).
	MarkOverflow bool
	// StderrLastLine makes a failure's CommandError carry the last line
	// of standard error, control and format characters stripped and at
	// most 200 bytes, instead of the first (amendment p: jail -r, whose stderr
	// starts with the jail's /etc/rc.shutdown's and ends with jail(8)'s).
	StderrLastLine bool
	// StderrTail, when set, receives after a successful run the last
	// MaxOutput bytes of standard error, preceded by a
	// "skep: N bytes discarded" line when more was written; nothing when
	// the command fails. A failed write to it is not an error (amendment q: jail -r, whose stderr is the jail's
	// /etc/rc.shutdown's). It is not used with ConsoleLog.
	StderrTail io.Writer
	// OutputOnFailure makes a failed or timed-out run still return what
	// it kept of standard output (the marked head and tail with
	// MarkOverflow) alongside the error, and still write the stderr tail
	// to StderrTail (amendment u: a failed jail -r appends rc.shutdown's
	// output as a successful one does). A timeout is then a
	// *TimeoutError. A run cancelled by its caller returns nothing.
	OutputOnFailure bool
	// ConsoleLog, when set, is an absolute path (created 0600, never
	// through a link) that is truncated and receives standard output and
	// standard error through a pipe the agent drains: the first
	// ConsoleLogHead bytes, then, if there was more, a marker and the last
	// ConsoleLogTail bytes. The pipe is closed when the command ends, so
	// no process it leaves running holds a descriptor on the host file
	// (amendment o, SR-01). RunEnv then returns no output.
	ConsoleLog string
}

// EnvRunner runs an EnvCommand. The real Runner implements it; so must a
// test's when it drives the template builder.
type EnvRunner interface {
	RunEnv(ctx context.Context, cmd EnvCommand) ([]byte, error)
}

var (
	// templatePackageRE is the shape of a base package name (ADR 0010
	// decision 7a).
	templatePackageRE = regexp.MustCompile(`^FreeBSD-[a-z0-9][a-z0-9-]*$`)
	// releaseRE is the shape of freebsd-version's output.
	releaseRE = regexp.MustCompile(`^[0-9]+\.[0-9]+-[A-Z0-9]+(-p[0-9]+)?$`)
	// userlandRE is the line of bin/freebsd-version that holds the
	// userland release.
	userlandRE = regexp.MustCompile(`^USERLAND_VERSION="([^"]*)"$`)
)

// ValidTemplatePackage reports whether s may be a template package name.
func ValidTemplatePackage(s string) bool { return templatePackageRE.MatchString(s) }

// ValidRelease reports whether s has the shape of a FreeBSD release, for
// example 15.1-RELEASE-p6.
func ValidRelease(s string) bool { return releaseRE.MatchString(s) }

func (h *host) envRunner() (EnvRunner, error) {
	er, ok := h.run.(EnvRunner)
	if !ok {
		return nil, errors.New("hostops: this runner cannot run commands with their own environment")
	}
	return er, nil
}

// pkgEnv is the whole environment of pkg (the step 6 decision, item 8):
// the two fixed values of decision 7a, and TMPDIR in the work area.
func pkgEnv(p TemplatePaths) []string {
	return []string{"PATH=/sbin:/bin:/usr/sbin:/usr/bin", "ASSUME_ALWAYS_YES=yes", "TMPDIR=" + p.Tmp}
}

// HostRelease implements Templates.
func (h *host) HostRelease(ctx context.Context) (string, error) {
	out, err := h.run.Run(ctx, freebsdVersionBinary, "-k")
	if err != nil {
		return "", err
	}
	rel := strings.TrimSpace(string(out))
	if !ValidRelease(rel) {
		return "", fmt.Errorf("hostops: freebsd-version -k printed %q", firstLine(out))
	}
	return rel, nil
}

// PkgbaseAvailable implements Templates: pkg is installed, and its
// repository files enable FreeBSD-base (package pkgrepo, the rules
// skep-setup's check uses too). Without that repository the build's
// "pkg install FreeBSD-..." finds nothing (seen on smith, 2026-11-22).
func (h *host) PkgbaseAvailable() (bool, string) {
	for _, p := range []string{pkgBootstrap, pkgBinary} {
		if !h.sys.Exists(p) {
			return false, p + " is not installed on this host"
		}
	}
	if enabled, where := pkgrepo.Base(h.rootFS); !enabled {
		why := "the FreeBSD-base repository is not enabled"
		if where != "" {
			why += " (" + where + ")"
		} else {
			why += " (no repository file defines it)"
		}
		return false, why + `; enable it with printf 'FreeBSD-base: { enabled: yes }\n' >> /usr/local/etc/pkg/repos/FreeBSD.conf, or use the dist source`
	}
	return true, ""
}

// DistAvailable implements Templates.
func (h *host) DistAvailable() (bool, string) {
	dir := h.conf.DistDir
	if dir == "" {
		return false, "storage.dist_dir is not set in the agent's configuration"
	}
	for _, name := range []string{DistArchive, DistManifest} {
		if !h.sys.Exists(filepath.Join(dir, name)) {
			return false, name + " is not in " + dir
		}
	}
	return true, ""
}

// PkgInstall implements Templates.
func (h *host) PkgInstall(ctx context.Context, pool, templateID string, packages []string) error {
	if len(packages) == 0 {
		return invalid("no template packages are configured")
	}
	for _, name := range packages {
		if !ValidTemplatePackage(name) {
			return invalid("invalid template package name %q", printable(name))
		}
	}
	er, err := h.envRunner()
	if err != nil {
		return err
	}
	p, err := h.TemplatePaths(ctx, pool, templateID)
	if err != nil {
		return err
	}
	args := []string{
		"--rootdir", p.Root,
		"-o", "PKG_CACHEDIR=" + p.Cache,
		"-o", "PKG_DBDIR=" + p.Root + "/var/db/pkg",
		// No package code runs on the host (item 1).
		"-o", "RUN_SCRIPTS=false",
		"-o", "PKG_TRIGGERS_ENABLE=false",
		"install", "-y",
	}
	_, err = er.RunEnv(ctx, EnvCommand{Path: pkgBootstrap, Args: append(args, packages...),
		Env: pkgEnv(p), Timeout: pkgInstallTimeout})
	// The first stderr line reaches the build's task error: cleaned
	// (amendment s).
	return cleanStderr(err)
}

// rawPackage is the part of pkg info --raw that the allowlist reads.
type rawPackage struct {
	Name       string              `json:"name"`
	Version    string              `json:"version"`
	Scripts    map[string]string   `json:"scripts"`
	LuaScripts map[string][]string `json:"lua_scripts"`
}

// PkgScripts implements Templates.
func (h *host) PkgScripts(ctx context.Context, pool, templateID string) ([]PackageScripts, error) {
	er, err := h.envRunner()
	if err != nil {
		return nil, err
	}
	p, err := h.TemplatePaths(ctx, pool, templateID)
	if err != nil {
		return nil, err
	}
	out, err := er.RunEnv(ctx, EnvCommand{Path: pkgBootstrap,
		Args: []string{"--rootdir", p.Root, "-o", "PKG_DBDIR=" + p.Root + "/var/db/pkg",
			"info", "--raw", "--raw-format", "json-compact", "-a"},
		Env: pkgEnv(p), Timeout: pkgInfoTimeout, MaxOutput: pkgInfoMaxOutput})
	if err != nil {
		// Cleaned for the build's task error (amendment s).
		return nil, cleanStderr(err)
	}
	return parsePkgRaw(out)
}

// parsePkgRaw decodes pkg info --raw --raw-format json-compact -a. pkg
// 2.9.2 prints one JSON array of manifests (the step 6 probe); one
// manifest per line is accepted as well, so an older or newer pkg does
// not silently yield nothing. Output that is neither is an error, as is a
// manifest without a name.
func parsePkgRaw(out []byte) ([]PackageScripts, error) {
	trimmed := bytes.TrimSpace(out)
	var raws []rawPackage
	if len(trimmed) == 0 {
		return nil, errors.New("hostops: pkg info --raw printed nothing")
	}
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &raws); err != nil {
			return nil, fmt.Errorf("hostops: pkg info --raw: %w", err)
		}
	} else {
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		for {
			var r rawPackage
			if err := dec.Decode(&r); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return nil, fmt.Errorf("hostops: pkg info --raw: %w", err)
			}
			raws = append(raws, r)
		}
	}
	out2 := make([]PackageScripts, 0, len(raws))
	for _, r := range raws {
		if r.Name == "" {
			return nil, errors.New("hostops: pkg info --raw: a package has no name")
		}
		out2 = append(out2, PackageScripts(r))
	}
	return out2, nil
}

// TemplateRelease implements Templates. bin/freebsd-version is a shell
// script; its USERLAND_VERSION line is read, never run.
func (h *host) TemplateRelease(ctx context.Context, pool, templateID string) (string, error) {
	r, _, err := h.openTemplateRoot(ctx, pool, templateID)
	if err != nil {
		return "", err
	}
	defer r.Close()
	if ok, err := lstatDirChain(r, "bin"); err != nil {
		return "", err
	} else if !ok {
		return "", errors.New("hostops: the template has no bin directory")
	}
	data, err := readRegular(r, "bin/freebsd-version", 1<<20)
	if err != nil {
		return "", err
	}
	return parseUserland(data)
}

func parseUserland(data []byte) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if m := userlandRE.FindStringSubmatch(strings.TrimSpace(sc.Text())); m != nil {
			if !ValidRelease(m[1]) {
				return "", fmt.Errorf("hostops: the template's USERLAND_VERSION %q is not a release", printable(m[1]))
			}
			return m[1], nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", errors.New("hostops: the template's bin/freebsd-version has no USERLAND_VERSION line")
}

// ExtractDist implements Templates.
func (h *host) ExtractDist(ctx context.Context, pool, templateID string) (string, error) {
	dir := h.conf.DistDir
	if dir == "" {
		return "", storageUnavailable("storage.dist_dir is not set in the agent's configuration")
	}
	if err := checkRootOwnedDir(dir); err != nil {
		return "", err
	}
	p, err := h.TemplatePaths(ctx, pool, templateID)
	if err != nil {
		return "", err
	}
	manifest, err := os.ReadFile(filepath.Join(dir, DistManifest))
	if err != nil {
		return "", storageUnavailable("cannot read " + DistManifest + " in " + dir)
	}
	want, err := manifestSHA256(manifest, DistArchive)
	if err != nil {
		return "", err
	}
	archive := filepath.Join(dir, DistArchive)
	got, err := fileSHA256(ctx, archive)
	if err != nil {
		return "", err
	}
	if got != want {
		return "", &models.Error{Code: models.CodeTemplateArchiveMismatch,
			Message: DistArchive + " in " + dir + " does not match its " + DistManifest,
			Details: map[string]any{"manifest_sha256": want, "archive_sha256": got}}
	}
	er, err := h.envRunner()
	if err != nil {
		return "", err
	}
	if err := extractArchive(ctx, er, archive, p.Root); err != nil {
		return "", err
	}
	return got, nil
}

// extractArchive runs tar -x of a checked archive into a template's root.
// bsdtar refuses absolute names and ".." and does not follow symbolic
// links it extracted (its secure defaults, without -P); -p keeps the
// owners, modes, and file flags of the base system. A failure's first
// stderr line is cleaned for the build's task error (amendment s).
func extractArchive(ctx context.Context, er EnvRunner, archive, root string) error {
	_, err := er.RunEnv(ctx, EnvCommand{Path: tarBinary, Args: []string{"-x", "-p", "-f", archive, "-C", root},
		Env: []string{"PATH=/sbin:/bin:/usr/sbin:/usr/bin", "LC_ALL=C"}, Timeout: tarTimeout})
	return cleanStderr(err)
}

// checkRootOwnedDir refuses a dist directory an unprivileged user could
// write into: its archive is trusted as the operator's.
func checkRootOwnedDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return storageUnavailable("the dist directory " + dir + " cannot be read")
	}
	uid, ok := ownerUID(fi)
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() || !ok || uid != 0 || fi.Mode().Perm()&0o022 != 0 {
		return storageUnavailable("the dist directory " + dir + " must be a directory owned by root and writable by root only")
	}
	return nil
}

// manifestSHA256 finds an archive's SHA-256 in a distribution MANIFEST:
// tab-separated lines of name, SHA-256, file count, and more.
func manifestSHA256(manifest []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(manifest))
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) >= 2 && f[0] == name {
			sum := strings.ToLower(strings.TrimSpace(f[1]))
			if b, err := hex.DecodeString(sum); err != nil || len(b) != sha256.Size {
				return "", storageUnavailable("the " + DistManifest + " entry of " + name + " has no valid SHA-256")
			}
			return sum, nil
		}
	}
	return "", storageUnavailable(DistManifest + " has no entry for " + name)
}

// fileSHA256 hashes a regular file, checking ctx between blocks.
func fileSHA256(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buf)
		hash.Write(buf[:n])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
