// Package pkgrepo reads pkg(8)'s repository files to tell whether the
// FreeBSD-base (pkgbase) repository is enabled. skep-setup's check and
// the agent's template sources both use it, so they give the same answer
// (they disagreed on smith, 2026-11-22: setup read the files and said
// disabled, the agent only checked that pkg was installed).
//
// It follows pkg's layering: /etc/pkg/*.conf, then
// /usr/local/etc/pkg/repos/*.conf, each directory in name order; a later
// definition of a repository overrides the keys it sets. A repository
// whose first definition does not set "enabled" is enabled (pkg's
// default); a later one that does not set it leaves it as it was. Within
// one file only the first block of a name and its first "enabled" count,
// as in pkg. It does not follow a REPOS_DIR set in pkg.conf or the
// environment.
//
// It reads the UCL forms pkg's files use: NAME: { ... }, NAME = { ... }
// and NAME { ... }, and # and /* */ comments. pkg 2.9 does not read // as
// a comment (checked on langrishe, 2026-11-24), so neither does this.
// tests/integration/pkgrepo has the comparison with pkg.
package pkgrepo

import (
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Dirs are the repository directories pkg reads by default, relative to /.
var Dirs = []string{"etc/pkg", "usr/local/etc/pkg/repos"}

// BaseName is the name of the pkgbase repository on FreeBSD 15.
const BaseName = "FreeBSD-base"

// EnableBase is the shell command skep-setup's check and the agent print
// to enable FreeBSD-base. It creates /usr/local/etc/pkg/repos first: a
// host installed from the distribution sets has no such directory (smith,
// 2026-11-23), and the append alone then fails. The file it appends to is
// read after /etc/pkg/FreeBSD.conf, so its "enabled: yes" wins.
const EnableBase = `mkdir -p /usr/local/etc/pkg/repos && printf 'FreeBSD-base: { enabled: yes }\n' >> /usr/local/etc/pkg/repos/FreeBSD.conf`

var (
	// blockRE finds a repository block: NAME: { ... }, NAME = { ... } or
	// NAME { ... } (UCL; the name may be quoted). Nested braces do not
	// occur in repository blocks.
	blockRE   = regexp.MustCompile(`(?s)"?([A-Za-z0-9_.-]+)"?\s*[:=]?\s*\{([^{}]*)\}`)
	enabledRE = regexp.MustCompile(`(?i)\benabled\s*[:=]?\s*"?(yes|true|on|no|false|off)"?`)
	commentRE = regexp.MustCompile(`/\*(?s:.*?)\*/|#[^\n]*`)
	// varRE is a ${VAR} expansion in a url; its braces are not blocks.
	varRE = regexp.MustCompile(`\$\{[A-Za-z0-9_]*\}`)
)

// Base reports whether FreeBSD-base is enabled in the repository files of
// fsys (a host's / , as from os.DirFS). where names the file that decided
// it, empty when no file defines the repository.
func Base(fsys fs.FS) (enabled bool, where string) {
	defined := false
	for _, dir := range Dirs {
		entries, err := fs.ReadDir(fsys, dir)
		if err != nil {
			continue
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".conf") && !e.IsDir() {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			file := path.Join(dir, n)
			bs, err := fs.ReadFile(fsys, file)
			if err != nil {
				continue
			}
			inFile := false
			for _, m := range blockRE.FindAllStringSubmatch(varRE.ReplaceAllString(commentRE.ReplaceAllString(string(bs), ""), ""), -1) {
				if m[1] != BaseName || inFile {
					continue
				}
				inFile = true
				v := enabledRE.FindStringSubmatch(m[2])
				switch {
				case v != nil:
					enabled = isYes(v[1])
					where = "/" + file
				case !defined:
					enabled = true
					where = "/" + file
				}
				defined = true
			}
		}
	}
	return enabled, where
}

func isYes(s string) bool {
	switch strings.ToLower(s) {
	case "yes", "true", "on":
		return true
	}
	return false
}
