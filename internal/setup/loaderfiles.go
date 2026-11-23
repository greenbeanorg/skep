package setup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The loader's defaults for the variables that name more files
// (/boot/defaults/loader.conf, FreeBSD 15.1).
const (
	defaultLoaderDirs  = "/boot/loader.conf.d"
	defaultLoaderLocal = "/boot/loader.conf.local"
	defaultsLoaderConf = "/boot/defaults/loader.conf"
)

// readLoader reads loader.conf and the files that override it, in the
// order of readConf in /boot/lua/config.lua, so that Get's "last one wins"
// is the loader's: each file followed by the files its own
// loader_conf_files names, then the *.conf files of loader_conf_dirs, then
// local_loader_conf_files. A file is read once. The loader reads a
// directory in no fixed order; here it is sorted. product_vars directories
// are not read.
func readLoader(root string) (RCConf, error) {
	var l RCConf
	var err error
	if l.Main, err = ReadShVars(hostPath(root, loaderConfPath)); err != nil {
		return l, err
	}
	seen := map[string]bool{loaderConfPath: true}
	var follow func(v ShVars) error
	read := func(name string) error {
		p := filepath.Join("/", name)
		if seen[p] {
			return nil
		}
		seen[p] = true
		// Stat first: opening a FIFO named here would block.
		if st, err := os.Stat(hostPath(root, p)); errors.Is(err, fs.ErrNotExist) || (err == nil && !st.Mode().IsRegular()) {
			return nil
		}
		v, err := ReadShVars(hostPath(root, p))
		if err != nil {
			return err
		}
		l.Overrides = append(l.Overrides, v)
		return follow(v)
	}
	follow = func(v ShVars) error {
		for _, name := range strings.Fields(v.Vals["loader_conf_files"]) {
			if err := read(name); err != nil {
				return err
			}
		}
		return nil
	}
	if err := follow(l.Main); err != nil {
		return l, err
	}
	dirs, ok := l.Get("loader_conf_dirs")
	if !ok {
		dirs = defaultLoaderDirs
	}
	for _, dir := range strings.Fields(dirs) {
		entries, err := os.ReadDir(hostPath(root, filepath.Join("/", dir)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return l, err
		}
		for _, e := range entries { // ReadDir sorts by name
			if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".conf") {
				if err := read(filepath.Join(dir, e.Name())); err != nil {
					return l, err
				}
			}
		}
	}
	local, ok := l.Get("local_loader_conf_files")
	if !ok {
		local = defaultLoaderLocal
	}
	for _, name := range strings.Fields(local) {
		if err := read(name); err != nil {
			return l, err
		}
	}
	return l, nil
}

// parseKenvNames takes the names from kenv(1)'s NAME="value" lines.
func parseKenvNames(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if name, _, ok := strings.Cut(line, "="); ok && tunableNameRE.MatchString(name) {
			names = append(names, name)
		}
	}
	return names
}

// staleVMMTunables are the hw.vmm.* variables in the kernel environment
// that no loader file sets any more: vmm keeps them until the next boot,
// and then loses them.
func staleVMMTunables(names []string, loader, defaults RCConf) []string {
	var out []string
	for _, n := range names {
		if !strings.HasPrefix(n, vmmTunablePrefix) {
			continue
		}
		_, inLoader := loader.Get(n)
		_, inDefaults := defaults.Get(n)
		if !inLoader && !inDefaults {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// staleTunables warns about them. It never changes the plan's actions or
// an exit code.
func (b *builder) staleTunables() {
	if b.f.KenvNames == nil {
		return
	}
	defaults, err := ReadShVars(hostPath(b.o.Root, defaultsLoaderConf))
	if err != nil {
		return
	}
	stale := staleVMMTunables(b.f.KenvNames(), b.loader, RCConf{Main: defaults})
	if len(stale) > 0 {
		b.warn("loader tunables", "in the kernel environment, in no loader file: "+strings.Join(stale, " "),
			"they apply until the next boot and are then lost; add them to /boot/loader.conf to keep them, or ignore this if they were removed on purpose")
	}
}
