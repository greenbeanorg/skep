package setup

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ShVars is a file of sh variable assignments (rc.conf, loader.conf), read
// without running it. Setup never sources a host file: it parses the plain
// assignments and treats anything else as a reason to edit nothing in that
// file (ADR 0012 Decision 2).
type ShVars struct {
	Path    string
	Vals    map[string]string
	Complex []string // lines that are not plain assignments
	Missing bool
}

var assignRE = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_.]*)=(.*)$`)

// ReadShVars reads path. A missing file is not an error (Missing is set).
func ReadShVars(path string) (ShVars, error) {
	v := ShVars{Path: path, Vals: map[string]string{}}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		v.Missing = true
		return v, nil
	}
	if err != nil {
		return v, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := assignRE.FindStringSubmatch(line)
		if m == nil {
			v.Complex = append(v.Complex, line)
			continue
		}
		val, ok := unquote(m[2])
		if !ok {
			v.Complex = append(v.Complex, line)
			continue
		}
		v.Vals[m[1]] = val // the last assignment wins, as in sh
	}
	return v, sc.Err()
}

// Editable reports whether setup may edit the file: it exists or may be
// created, and every line is a plain assignment.
func (v ShVars) Editable() bool { return len(v.Complex) == 0 }

// unquote accepts "x", 'x', or a bare word with no shell syntax, and an
// unquoted trailing comment.
func unquote(s string) (string, bool) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, `"`):
		end := strings.Index(s[1:], `"`)
		if end < 0 {
			return "", false
		}
		in, rest := s[1:1+end], strings.TrimSpace(s[2+end:])
		if strings.ContainsAny(in, "$`\\") || (rest != "" && !strings.HasPrefix(rest, "#")) {
			return "", false
		}
		return in, true
	case strings.HasPrefix(s, "'"):
		end := strings.Index(s[1:], "'")
		if end < 0 {
			return "", false
		}
		in, rest := s[1:1+end], strings.TrimSpace(s[2+end:])
		if rest != "" && !strings.HasPrefix(rest, "#") {
			return "", false
		}
		return in, true
	default:
		if i := strings.Index(s, "#"); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
		return s, !strings.ContainsAny(s, "$`\\\"' ;&|()<>")
	}
}

// RCConf is the host's rc.conf with the files that can override it.
type RCConf struct {
	Main ShVars
	// Overrides are rc.conf.local and every file in rc.conf.d: a
	// variable set there wins over rc.conf, so setup will not add it to
	// rc.conf.
	Overrides []ShVars
}

// ReadRCConf reads <root>/etc/rc.conf, rc.conf.local, and rc.conf.d/*.
func ReadRCConf(root string) (RCConf, error) {
	var rc RCConf
	var err error
	if rc.Main, err = ReadShVars(filepath.Join(root, "etc/rc.conf")); err != nil {
		return rc, err
	}
	local, err := ReadShVars(filepath.Join(root, "etc/rc.conf.local"))
	if err != nil {
		return rc, err
	}
	if !local.Missing {
		rc.Overrides = append(rc.Overrides, local)
	}
	entries, err := os.ReadDir(filepath.Join(root, "etc/rc.conf.d"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return rc, err
	}
	names := []string{}
	for _, e := range entries {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		v, err := ReadShVars(filepath.Join(root, "etc/rc.conf.d", n))
		if err != nil {
			return rc, err
		}
		rc.Overrides = append(rc.Overrides, v)
	}
	return rc, nil
}

// Get returns a variable's effective value: an override wins.
func (rc RCConf) Get(name string) (string, bool) {
	for i := len(rc.Overrides) - 1; i >= 0; i-- {
		if v, ok := rc.Overrides[i].Vals[name]; ok {
			return v, true
		}
	}
	v, ok := rc.Main.Vals[name]
	return v, ok
}

// OverriddenIn names the override file that sets name, or "".
func (rc RCConf) OverriddenIn(name string) string {
	for _, o := range rc.Overrides {
		if _, ok := o.Vals[name]; ok {
			return o.Path
		}
	}
	return ""
}

// ComplexOverride names an override file setup cannot read fully, or "".
func (rc RCConf) ComplexOverride() string {
	for _, o := range rc.Overrides {
		if len(o.Complex) > 0 {
			return o.Path
		}
	}
	return ""
}

// Account is one line of passwd or group.
type Account struct {
	Name  string
	ID    string
	Home  string // passwd only
	Shell string // passwd only
}

// ReadAccounts reads <root>/etc/passwd or <root>/etc/group.
func ReadAccounts(path string) ([]Account, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Account
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) < 3 {
			return nil, fmt.Errorf("%s: malformed line %q", path, line)
		}
		a := Account{Name: f[0], ID: f[2]}
		if len(f) >= 7 {
			a.Home, a.Shell = f[5], f[6]
		}
		out = append(out, a)
	}
	return out, nil
}

func findAccount(list []Account, name string) (Account, bool) {
	for _, a := range list {
		if a.Name == name {
			return a, true
		}
	}
	return Account{}, false
}

func findAccountID(list []Account, id string) (Account, bool) {
	for _, a := range list {
		if a.ID == id {
			return a, true
		}
	}
	return Account{}, false
}
