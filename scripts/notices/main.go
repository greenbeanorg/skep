// Command notices writes THIRD_PARTY_NOTICES: the license texts of every Go
// module linked into the shipped binaries and every npm package whose code
// is in the built web interface.
//
// Run it from the repository root with "make notices"; "make notices-check"
// fails when the committed file is stale. It needs the Go module cache
// ("go mod download") and web/node_modules ("make web-install"), and uses
// only the standard library and the go command.
//
// What counts as shipped:
//
//   - Go: the packages "go list -deps" reports for the shipped commands,
//     built for each release target with the release build tags, grouped
//     by module, plus the Go standard library and runtime.
//   - npm: every package in web/package-lock.json that is neither dev nor
//     devOptional (the runtime dependencies and what they pull in; types
//     are erased and never bundled), plus the build tools whose generated
//     code is in the bundle (buildTools), plus the shadcn/ui components
//     copied into web/src/components/ui. This can list a package that
//     tree shaking drops; it never misses one that the runtime
//     dependencies pull in.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// shippedCommands are the binaries a release installs.
var shippedCommands = []string{"./cmd/skep-agent", "./cmd/skep-api", "./cmd/skepctl"}

// releaseTargets are the GOOS/GOARCH pairs releases are built for.
var releaseTargets = [][2]string{{"freebsd", "amd64"}}

// releaseTags are the build tags of the release build (skep_webui embeds
// the web interface in skep-api).
var releaseTags = "skep_webui"

// webuiPackage and missingDist name the one go list error tolerated: the
// embedded interface not built yet.
const (
	webuiPackage = "github.com/greenbeanorg/skep/internal/webui"
	missingDist  = "pattern all:dist: no matching files found"
)

// buildTools are devDependencies whose output is in the built interface:
// Tailwind CSS generates the stylesheet (with its preflight), and Vite
// injects its module preload helper.
var buildTools = []string{"tailwindcss", "vite"}

// licenseFile matches the files at a package's root that are copied in.
// AUTHORS is included because some licenses name "the authors" in it.
var licenseFile = regexp.MustCompile(`(?i)^(licen[cs]e|copying|notice|patents|authors)([-._].*)?$`)

// Notice is one component and its license files.
type Notice struct {
	Name    string
	Version string
	// Note says why a component is listed when that is not obvious.
	Note  string
	Files []LicenseFile
}

// Title is the component's name, version and note on one line.
func (n Notice) Title() string {
	t := n.Name
	if n.Version != "" {
		t += " " + n.Version
	}
	if n.Note != "" {
		t += " (" + n.Note + ")"
	}
	return t
}

// LicenseFile is one license file's name and text.
type LicenseFile struct {
	Name string
	Text string
}

func main() {
	out := flag.String("o", "THIRD_PARTY_NOTICES", "the file to write, or - for standard output")
	check := flag.Bool("check", false, "compare with the file instead of writing it; exit 1 when it differs")
	flag.Parse()
	if err := run(*out, *check); err != nil {
		fmt.Fprintln(os.Stderr, "notices:", err)
		os.Exit(1)
	}
}

func run(out string, check bool) error {
	goNotices, err := goModules()
	if err != nil {
		return err
	}
	npmNotices, err := npmPackages("web")
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	render(&buf, goNotices, npmNotices)
	if check {
		have, err := os.ReadFile(out)
		if err != nil {
			return err
		}
		if !bytes.Equal(have, buf.Bytes()) {
			return fmt.Errorf("%s is stale: run \"make notices\" and commit it", out)
		}
		return nil
	}
	if out == "-" {
		_, err := os.Stdout.Write(buf.Bytes())
		return err
	}
	return os.WriteFile(out, buf.Bytes(), 0o644)
}

// goModules lists the modules linked into the shipped commands, and the
// standard library.
func goModules() ([]Notice, error) {
	type mod struct{ path, version, dir string }
	mods := map[string]mod{}
	for _, t := range releaseTargets {
		// -e: without "make web-build" first, internal/webui/dist is
		// missing and the embed pattern fails; the import graph is still
		// complete. Any other package error is fatal.
		args := append([]string{"list", "-e", "-deps", "-tags", releaseTags,
			"-f", "{{if .Error}}ERROR\t{{.ImportPath}}\t{{.Error}}\n{{end}}" +
				"{{with .Module}}{{if not .Main}}{{.Path}}\t{{.Version}}\t{{.Dir}}{{end}}{{end}}"}, shippedCommands...)
		cmd := exec.Command("go", args...)
		cmd.Env = append(os.Environ(), "GOOS="+t[0], "GOARCH="+t[1], "CGO_ENABLED=0")
		cmd.Stderr = os.Stderr
		b, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list for %s/%s: %w", t[0], t[1], err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Split(line, "\t")
			if len(f) == 3 && f[0] == "ERROR" {
				if f[1] == webuiPackage && strings.Contains(f[2], missingDist) {
					continue
				}
				return nil, fmt.Errorf("go list for %s/%s: %s: %s", t[0], t[1], f[1], f[2])
			}
			if len(f) != 3 {
				continue
			}
			if f[2] == "" {
				return nil, fmt.Errorf("module %s %s is not in the module cache: run \"go mod download\"", f[0], f[1])
			}
			mods[f[0]] = mod{f[0], f[1], f[2]}
		}
	}
	goroot, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return nil, fmt.Errorf("go env GOROOT: %w", err)
	}
	std, err := licenseFiles(strings.TrimSpace(string(goroot)), "Go standard library and runtime")
	if err != nil {
		return nil, err
	}
	// The toolchain version is left out so the file does not change with
	// a patch release of Go; the license text does not.
	out := []Notice{{Name: "Go standard library and runtime", Note: "golang.org", Files: std}}
	paths := make([]string, 0, len(mods))
	for p := range mods {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		m := mods[p]
		files, err := licenseFiles(m.dir, m.path)
		if err != nil {
			return nil, err
		}
		out = append(out, Notice{Name: m.path, Version: m.version, Files: files})
	}
	return out, nil
}

// lockfile is the part of package-lock.json (version 2 or 3) read here.
type lockfile struct {
	LockfileVersion int `json:"lockfileVersion"`
	Packages        map[string]struct {
		Version     string `json:"version"`
		Dev         bool   `json:"dev"`
		DevOptional bool   `json:"devOptional"`
		Link        bool   `json:"link"`
	} `json:"packages"`
}

// npmPackages lists the packages whose code is in the built interface.
func npmPackages(webDir string) ([]Notice, error) {
	b, err := os.ReadFile(filepath.Join(webDir, "package-lock.json"))
	if err != nil {
		return nil, err
	}
	var lock lockfile
	if err := json.Unmarshal(b, &lock); err != nil {
		return nil, fmt.Errorf("package-lock.json: %w", err)
	}
	if lock.LockfileVersion < 2 {
		return nil, fmt.Errorf("package-lock.json: lockfile version %d has no packages map", lock.LockfileVersion)
	}
	tools := map[string]bool{}
	for _, t := range buildTools {
		tools["node_modules/"+t] = true
	}
	var keys []string
	for k, p := range lock.Packages {
		if k == "" || p.Link || !strings.HasPrefix(k, "node_modules/") {
			continue
		}
		if (p.Dev || p.DevOptional) && !tools[k] {
			continue
		}
		keys = append(keys, k)
	}
	for k := range tools {
		if _, ok := lock.Packages[k]; !ok {
			return nil, fmt.Errorf("package-lock.json has no %s", k)
		}
	}
	sort.Strings(keys)
	var out []Notice
	for _, k := range keys {
		name := k[strings.LastIndex(k, "node_modules/")+len("node_modules/"):]
		files, err := licenseFiles(filepath.Join(webDir, filepath.FromSlash(k)), name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%s is not installed: run \"make web-install\"", k)
			}
			return nil, err
		}
		n := Notice{Name: name, Version: lock.Packages[k].Version, Files: files}
		if strings.Count(k, "node_modules/") > 1 {
			n.Note = "nested in " + strings.TrimPrefix(k[:strings.LastIndex(k, "/node_modules/")], "node_modules/")
		}
		if tools[k] {
			n.Note = "build tool; its generated code is in the bundle"
		}
		out = append(out, n)
	}
	shadcn, err := licenseFiles(filepath.Join("scripts", "notices", "vendored", "shadcn-ui"), "shadcn/ui")
	if err != nil {
		return nil, err
	}
	out = append(out, Notice{Name: "shadcn/ui", Note: "components copied into web/src/components/ui", Files: shadcn})
	return out, nil
}

// licenseFiles reads the license files at the root of dir. A component
// without one is an error: its terms cannot be passed on.
func licenseFiles(dir, what string) ([]LicenseFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []LicenseFile
	for _, e := range entries {
		if !e.Type().IsRegular() || !licenseFile.MatchString(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, LicenseFile{Name: e.Name(), Text: normalize(string(b))})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s (%s) has no license file", what, dir)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// normalize makes line endings LF and ends the text with one newline.
func normalize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.TrimRight(s, "\n\t ") + "\n"
}

func render(w io.Writer, goNotices, npmNotices []Notice) {
	fmt.Fprint(w, `THIRD-PARTY SOFTWARE NOTICES FOR SKEP

Skep includes the third-party software listed below. Each component is
distributed under its own license, reproduced in full after its name.

This file is generated by "make notices" (scripts/notices). Do not edit
it by hand; CI fails when it is stale.

Part 1 lists the Go modules linked into skep-agent, skep-api and skepctl
as built for release (FreeBSD amd64, with the web interface embedded).
Part 2 lists the npm packages whose code is in the web interface that
skep-api serves.

`)
	section := func(title string, ns []Notice) {
		fmt.Fprintf(w, "%s\n%s\n\n", title, strings.Repeat("=", len(title)))
		for _, n := range ns {
			fmt.Fprintf(w, "  - %s\n", n.Title())
		}
		fmt.Fprintln(w)
		for _, n := range ns {
			fmt.Fprintf(w, "%s\n%s\n\n", strings.Repeat("-", 72), n.Title())
			for _, f := range n.Files {
				fmt.Fprintf(w, "--- %s ---\n\n%s\n", f.Name, f.Text)
			}
		}
		fmt.Fprintln(w)
	}
	section("Part 1: Go modules", goNotices)
	section("Part 2: npm packages in the web interface", npmNotices)
}
