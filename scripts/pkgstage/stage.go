package main

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Package metadata fixed by ADR 0012 Decision 1.
const (
	pkgName    = "skep"
	pkgOrigin  = "sysutils/skep"
	pkgABI     = "FreeBSD:15:amd64"
	pkgPrefix  = "/usr/local"
	pkgLicense = "BSD2CLAUSE"
	pkgWWW     = "https://github.com/greenbeanorg/skep"
	// marker names a directory this tool made, so a rerun may replace it
	// and nothing else is ever removed.
	marker = ".skep-pkgstage"
)

// entry is one file of the package: where it comes from, where it goes
// under the prefix, and its owner and mode in the package.
type entry struct {
	src   string // relative to the repository, or to the binary directory
	bin   bool   // src is in the binary directory
	dst   string // relative to the prefix
	owner string
	group string
	mode  os.FileMode
}

// layout is the package's content (ADR 0012 Decision 1). It never
// contains agent.json or api.json, so an upgrade cannot overwrite the
// administrator's configuration.
var layout = []entry{
	{src: "skep-agent", bin: true, dst: "sbin/skep-agent", owner: "root", group: "wheel", mode: 0o555},
	{src: "skep-api", bin: true, dst: "sbin/skep-api", owner: "root", group: "wheel", mode: 0o555},
	{src: "skep-setup", bin: true, dst: "sbin/skep-setup", owner: "root", group: "wheel", mode: 0o555},
	{src: "skepctl", bin: true, dst: "bin/skepctl", owner: "root", group: "wheel", mode: 0o555},
	{src: "packaging/rc.d/skep_agent", dst: "etc/rc.d/skep_agent", owner: "root", group: "wheel", mode: 0o555},
	{src: "packaging/rc.d/skep_api", dst: "etc/rc.d/skep_api", owner: "root", group: "wheel", mode: 0o555},
	{src: "packaging/etc/agent.json.sample", dst: "etc/skep/agent.json.sample", owner: "root", group: "wheel", mode: 0o644},
	{src: "packaging/etc/api.json.sample", dst: "etc/skep/api.json.sample", owner: "root", group: "wheel", mode: 0o644},
	{src: "LICENSE", dst: "share/licenses/skep/LICENSE", owner: "root", group: "wheel", mode: 0o444},
	{src: "THIRD_PARTY_NOTICES", dst: "share/licenses/skep/THIRD_PARTY_NOTICES", owner: "root", group: "wheel", mode: 0o444},
}

// scripts are the package scripts, embedded in the manifest.
var scripts = map[string]string{
	"pre-install":   "packaging/pkg/pre-install.sh",
	"pre-deinstall": "packaging/pkg/pre-deinstall.sh",
}

// Options says where to read from and write to.
type Options struct {
	Repo    string // the repository root
	BinDir  string // the FreeBSD amd64 binaries
	Out     string // the output directory
	Version string
	// CheckELF requires every binary to be a FreeBSD amd64 ELF file.
	CheckELF bool
}

var versionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([._][A-Za-z0-9]+)*$`)

// Result is what Stage wrote.
type Result struct {
	Stage    string
	Manifest string
	Plist    string
	Sums     string
}

// Stage writes the package tree, its manifest, and its plist under
// o.Out. It refuses an output directory that exists and was not made by
// this tool.
func Stage(o Options) (Result, error) {
	if !versionRE.MatchString(o.Version) {
		return Result{}, fmt.Errorf("version %q is not a package version such as 0.1.0", o.Version)
	}
	if err := prepareOut(o.Out); err != nil {
		return Result{}, err
	}
	stage := filepath.Join(o.Out, "stage")
	root := filepath.Join(stage, strings.TrimPrefix(pkgPrefix, "/"))
	var plist bytes.Buffer
	var sums []string
	for _, e := range layout {
		src := filepath.Join(o.Repo, e.src)
		if e.bin {
			src = filepath.Join(o.BinDir, e.src)
			if o.CheckELF {
				if err := checkELF(src); err != nil {
					return Result{}, err
				}
			}
		}
		dst := filepath.Join(root, e.dst)
		sum, err := copyFile(src, dst, e.mode)
		if err != nil {
			return Result{}, err
		}
		fmt.Fprintf(&plist, "@(%s,%s,%04o) %s\n", e.owner, e.group, e.mode, e.dst)
		sums = append(sums, fmt.Sprintf("%s  %s", sum, filepath.ToSlash(filepath.Join(pkgPrefix, e.dst))))
	}
	manifest, err := Manifest(o.Repo, o.Version)
	if err != nil {
		return Result{}, err
	}
	r := Result{
		Stage:    stage,
		Manifest: filepath.Join(o.Out, "+MANIFEST"),
		Plist:    filepath.Join(o.Out, "plist"),
		Sums:     filepath.Join(o.Out, "SHA256SUMS.stage"),
	}
	sort.Strings(sums)
	for path, data := range map[string][]byte{
		r.Manifest: manifest,
		r.Plist:    plist.Bytes(),
		r.Sums:     []byte(strings.Join(sums, "\n") + "\n"),
	} {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return Result{}, err
		}
	}
	return r, nil
}

// prepareOut makes an empty output directory. An existing one is
// replaced only if this tool made it.
func prepareOut(out string) error {
	if out == "" || filepath.Clean(out) == "/" {
		return errors.New("refusing an empty or root output directory")
	}
	entries, err := os.ReadDir(out)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	case len(entries) > 0:
		if _, err := os.Stat(filepath.Join(out, marker)); err != nil {
			return fmt.Errorf("%s exists and was not made by pkgstage; remove it yourself", out)
		}
		if err := os.RemoveAll(out); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, marker), nil, 0o644)
}

func copyFile(src, dst string, mode os.FileMode) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	if st, err := in.Stat(); err != nil {
		return "", err
	} else if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", src)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), in); err != nil {
		out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(dst, mode); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checkELF requires a FreeBSD amd64 executable. The package ABI comes
// from the manifest, not from the binary: Go stamps a FreeBSD 12.3 note
// into its binaries, so pkg's detection would get the wrong version.
func checkELF(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("%s: not an ELF file: %w", path, err)
	}
	defer f.Close()
	if f.Machine != elf.EM_X86_64 || f.OSABI != elf.ELFOSABI_FREEBSD {
		return fmt.Errorf("%s: %s %s, want a FreeBSD amd64 binary (make build-freebsd)", path, f.OSABI, f.Machine)
	}
	return nil
}

// manifest is +MANIFEST. pkg reads UCL, of which JSON is a subset.
type manifest struct {
	Name         string            `json:"name"`
	Origin       string            `json:"origin"`
	Version      string            `json:"version"`
	Comment      string            `json:"comment"`
	Desc         string            `json:"desc"`
	WWW          string            `json:"www"`
	Maintainer   string            `json:"maintainer"`
	ABI          string            `json:"abi"`
	Prefix       string            `json:"prefix"`
	LicenseLogic string            `json:"licenselogic"`
	Licenses     []string          `json:"licenses"`
	Categories   []string          `json:"categories"`
	Scripts      map[string]string `json:"scripts"`
}

// Manifest returns +MANIFEST for the version, with the scripts embedded.
func Manifest(repo, version string) ([]byte, error) {
	m := manifest{
		Name:    pkgName,
		Origin:  pkgOrigin,
		Version: version,
		Comment: "Virtualization and infrastructure management platform",
		Desc: "Skep manages bhyve virtual machines and jails on a FreeBSD host through a web\n" +
			"interface, a REST API, and the skepctl command line.\n\n" +
			"The package installs files only. Run skep-setup check, then skep-setup plan\n" +
			"and skep-setup apply, to configure the host.",
		WWW: pkgWWW,
		// No personal address in the package: the project page is the
		// contact.
		Maintainer:   pkgWWW,
		ABI:          pkgABI,
		Prefix:       pkgPrefix,
		LicenseLogic: "single",
		Licenses:     []string{pkgLicense},
		Categories:   []string{"sysutils"},
		Scripts:      map[string]string{},
	}
	for name, path := range scripts {
		b, err := os.ReadFile(filepath.Join(repo, path))
		if err != nil {
			return nil, err
		}
		m.Scripts[name] = string(b)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
