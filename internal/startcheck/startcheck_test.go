package startcheck

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

const (
	posixOK = "user::rwx\ngroup::r-x\nother::r-x\n"
	nfs4OK  = "            owner@:rwxp--aARWcCos:-------:allow\n            group@:r-x---a-R-c--s:-------:allow\n         everyone@:r-x---a-R-c--s:-------:allow\n"
)

type fakeFS struct {
	st      map[string]Info
	links   map[string]string
	acl     map[string]string
	aclErr  map[string]error
	statErr map[string]error
	aclAsk  []string
}

func newFake(paths ...string) *fakeFS {
	f := &fakeFS{st: map[string]Info{}, links: map[string]string{}, acl: map[string]string{}, aclErr: map[string]error{}, statErr: map[string]error{}}
	for _, p := range paths {
		for _, c := range components(p) {
			f.st[c] = Info{Perm: 0o755}
		}
	}
	return f
}

func (f *fakeFS) Lstat(p string) (Info, error) {
	if err := f.statErr[p]; err != nil {
		return Info{}, err
	}
	st, ok := f.st[p]
	if !ok {
		return Info{}, fs.ErrNotExist
	}
	return st, nil
}

func (f *fakeFS) Readlink(p string) (string, error) {
	to, ok := f.links[p]
	if !ok {
		return "", errors.New("not a link")
	}
	return to, nil
}

func (f *fakeFS) ACL(p string) (string, error) {
	f.aclAsk = append(f.aclAsk, p)
	if err := f.aclErr[p]; err != nil {
		return "", err
	}
	if a, ok := f.acl[p]; ok {
		return a, nil
	}
	return posixOK, nil
}

// components returns /, /a, /a/b for /a/b.
func components(path string) []string {
	out := []string{"/"}
	cur := ""
	for _, part := range strings.Split(strings.Trim(path, "/"), "/") {
		if part == "" {
			continue
		}
		cur += "/" + part
		out = append(out, cur)
	}
	return out
}

func refusal(t *testing.T, err error) *Refusal {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("want a refusal, got %v", err)
	}
	return r
}

func bin() Target { return Target{Path: "/usr/local/sbin/skep-agent", What: "the agent's binary"} }

func TestCheckPasses(t *testing.T) {
	f := newFake("/usr/local/sbin/skep-agent", "/usr/local/etc/skep/agent.json")
	if err := Check(f, []Target{bin(), {Path: "/usr/local/etc/skep/agent.json", What: "c"}}); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.Join(f.aclAsk, " "), "/usr/local "); n > 1 {
		t.Errorf("a shared parent was checked %d times", n)
	}
}

func TestCheckRefusesOwnerAndMode(t *testing.T) {
	for _, p := range []string{"/", "/usr", "/usr/local/sbin", "/usr/local/sbin/skep-agent"} {
		for _, c := range []struct {
			st  Info
			why string
			fix string
		}{
			{Info{UID: 1002, Perm: 0o755}, "owned by UID 1002", "chown root:wheel " + p},
			{Info{Perm: 0o775}, "writable by its group", "chmod go-w " + p},
			{Info{Perm: 0o757}, "writable by its group or others", "chmod go-w " + p},
			{Info{Perm: 0o777}, "mode 0777", "chmod go-w " + p},
		} {
			f := newFake("/usr/local/sbin/skep-agent")
			f.st[p] = c.st
			r := refusal(t, Check(f, []Target{bin()}))
			if r.Path != p || !strings.Contains(r.Why, c.why) || r.Fix != c.fix {
				t.Errorf("%s %+v: %v", p, c.st, r)
			}
		}
	}
}

func TestCheckFirstFailureIsOutermost(t *testing.T) {
	f := newFake("/usr/local/sbin/skep-agent")
	f.st["/usr"] = Info{Perm: 0o777}
	f.st["/usr/local"] = Info{UID: 5, Perm: 0o755}
	if r := refusal(t, Check(f, []Target{bin()})); r.Path != "/usr" {
		t.Errorf("first failure %s", r.Path)
	}
}

func TestCheckACLs(t *testing.T) {
	for name, c := range map[string]struct {
		out  string
		err  error
		want string
	}{
		"posix trivial":        {out: posixOK},
		"nfs4 trivial":         {out: nfs4OK},
		"posix named user":     {out: "user::rwx\nuser:1001:rwx\ngroup::r-x\nmask::rwx\nother::r-x\n", want: "beyond its mode"},
		"posix group write":    {out: "user::rwx\ngroup::rwx\nother::r-x\n", want: "beyond its mode"},
		"nfs4 named user":      {out: nfs4OK + "user:1001:rwxp--aARWcCos:-------:allow\n", want: "beyond its mode"},
		"nfs4 group write":     {out: "owner@:rwxp--aARWcCos:-------:allow\ngroup@:rwxp--aARWcCos:-------:allow\neveryone@:r-x---a-R-c--s:-------:allow\n", want: "beyond its mode"},
		"nfs4 everyone acl":    {out: "owner@:rwxp--aARWcCos:-------:allow\ngroup@:r-x---a-R-c--s:-------:allow\neveryone@:r-x---a-R-cC-s:-------:allow\n", want: "beyond its mode"},
		"nfs4 everyone owner":  {out: "owner@:rwxp--aARWcCos:-------:allow\ngroup@:r-x---a-R-c--s:-------:allow\neveryone@:r-x---a-R-c-os:-------:allow\n", want: "beyond its mode"},
		"nfs4 everyone delete": {out: "owner@:rwxp--aARWcCos:-------:allow\ngroup@:r-x---a-R-c--s:-------:allow\neveryone@:r-xpd-a-R-c--s:-------:allow\n", want: "beyond its mode"},
		"nfs4 inherited":       {out: "owner@:rwxp--aARWcCos:fd-----:allow\ngroup@:r-x---a-R-c--s:-------:allow\neveryone@:r-x---a-R-c--s:-------:allow\n", want: "beyond its mode"},
		"nfs4 deny":            {out: "owner@:rwxp--aARWcCos:-------:allow\ngroup@:r-x---a-R-c--s:-------:deny\neveryone@:r-x---a-R-c--s:-------:allow\n", want: "deny entries"},
		"duplicate class":      {out: "user::rwx\nuser::rwx\nother::r-x\n", want: "beyond its mode"},
		"mixed formats":        {out: "user::rwx\ngroup@:r-x---a-R-c--s:-------:allow\nother::r-x\n", want: "beyond its mode"},
		"empty":                {out: "", want: "does not understand"},
		"garbage":              {out: "getfacl: something\n", want: "does not understand"},
		"one bad line":         {out: "user::rwx\ngroup::r-x\nother::r-x\nnonsense\n", want: "does not understand"},
		"getfacl fails":        {err: errors.New("exit status 1"), want: "could not have its ACL read"},
	} {
		f := newFake("/usr/local/sbin/skep-agent")
		f.acl["/usr/local/sbin/skep-agent"] = c.out
		if c.err != nil {
			f.aclErr["/usr/local/sbin/skep-agent"] = c.err
		}
		err := Check(f, []Target{bin()})
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if r := refusal(t, err); r.Path != "/usr/local/sbin/skep-agent" || !strings.Contains(r.Why, c.want) {
			t.Errorf("%s: %v", name, r)
		}
	}
}

// zfs0705 is getfacl -q -n for mode 0705 on ZFS (langrishe, 2026-11-23).
const zfs0705 = `            owner@:r-x-----------:-------:allow
            group@:r-x-----------:-------:deny
            owner@:rwxp--aARWcCos:-------:allow
            group@:------a-R-c--s:-------:allow
         everyone@:r-x---a-R-c--s:-------:allow
`

func TestCheckModeDenyEntriesFix(t *testing.T) {
	const p = "/usr/local/sbin/skep-agent"
	f := newFake(p)
	f.st[p] = Info{Perm: 0o705}
	f.acl[p] = zfs0705
	r := refusal(t, Check(f, []Target{bin()}))
	for _, want := range []string{"deny entries", "0705"} {
		if !strings.Contains(r.Why, want) {
			t.Errorf("why lacks %q: %s", want, r.Why)
		}
	}
	for _, want := range []string{"0755", "0700", "chmod"} {
		if !strings.Contains(r.Fix, want) {
			t.Errorf("fix lacks %q: %s", want, r.Fix)
		}
	}
	if strings.Contains(r.Error(), "setfacl") {
		t.Errorf("a mode's deny entries are not fixed with setfacl: %v", r)
	}
}

// A deny entry alongside something a mode does not produce keeps the
// advice to review and remove the ACL: a new mode may not clear it.
func TestCheckSetDenyACLKeepsSetfaclFix(t *testing.T) {
	const p = "/usr/local/sbin/skep-agent"
	for name, out := range map[string]string{
		"named user allow": zfs0705 + "user:1001:rwxp--aARWcCos:-------:allow\n",
		"named user deny":  zfs0705 + "user:1001:r-x-----------:-------:deny\n",
		"inherited deny":   strings.Replace(zfs0705, "r-x-----------:-------:deny", "r-x-----------:fd-----:deny", 1),
		"group write":      strings.Replace(zfs0705, "group@:------a-R-c--s", "group@:-w----a-R-c--s", 1),
		"everyone acl":     strings.Replace(zfs0705, "everyone@:r-x---a-R-c--s", "everyone@:r-x---a-R-cC-s", 1),
	} {
		f := newFake(p)
		f.acl[p] = out
		r := refusal(t, Check(f, []Target{bin()}))
		if !strings.Contains(r.Why, "beyond its mode") || !strings.Contains(r.Fix, "setfacl -b") {
			t.Errorf("%s: %v", name, r)
		}
	}
}

func TestCheckSymlinkChecksBothChains(t *testing.T) {
	f := newFake("/usr/local/etc/skep", "/data/conf/agent.json")
	f.st["/usr/local/etc/skep/agent.json"] = Info{UID: 1002, Perm: 0o777, Symlink: true}
	f.links["/usr/local/etc/skep/agent.json"] = "/data/conf/agent.json"
	tgt := Target{Path: "/usr/local/etc/skep/agent.json", What: "c"}
	if err := Check(f, []Target{tgt}); err != nil {
		t.Fatalf("a link's own owner and mode do not matter: %v", err)
	}
	f = newFake("/usr/local/etc/skep", "/data/conf/agent.json")
	f.st["/usr/local/etc/skep/agent.json"] = Info{Symlink: true}
	f.links["/usr/local/etc/skep/agent.json"] = "/data/conf/agent.json"
	f.st["/data/conf"] = Info{Perm: 0o777}
	if r := refusal(t, Check(f, []Target{tgt})); r.Path != "/data/conf" {
		t.Errorf("resolved chain: %v", r)
	}
	f = newFake("/usr/local/etc/skep", "/data/conf/agent.json")
	f.st["/usr/local/etc/skep/agent.json"] = Info{Symlink: true}
	f.links["/usr/local/etc/skep/agent.json"] = "/data/conf/agent.json"
	f.st["/usr/local/etc/skep"] = Info{Perm: 0o775}
	if r := refusal(t, Check(f, []Target{tgt})); r.Path != "/usr/local/etc/skep" {
		t.Errorf("the directory holding the link: %v", r)
	}
}

func link(f *fakeFS, from, to string) {
	f.st[from] = Info{Symlink: true}
	f.links[from] = to
}

// A config that is a link to a link: the directory of the middle link is
// neither the first path's nor the final file's.
func TestCheckTwoHopLinkDirectory(t *testing.T) {
	f := newFake("/etc/skep", "/hop", "/real/agent.json")
	link(f, "/etc/skep/agent.json", "/hop/next.json")
	link(f, "/hop/next.json", "/real/agent.json")
	tgt := Target{Path: "/etc/skep/agent.json", What: "c"}
	if err := Check(f, []Target{tgt}); err != nil {
		t.Fatalf("clean chain: %v", err)
	}
	f.st["/hop"] = Info{Perm: 0o775}
	if r := refusal(t, Check(f, []Target{tgt})); r.Path != "/hop" || !strings.Contains(r.Why, "writable") {
		t.Errorf("want /hop named: %v", r)
	}
}

func TestCheckLinkInTheMiddle(t *testing.T) {
	f := newFake("/usr/local", "/data/etc/agent.json")
	link(f, "/usr/local/etc", "/data/etc")
	tgt := Target{Path: "/usr/local/etc/agent.json", What: "c"}
	if err := Check(f, []Target{tgt}); err != nil {
		t.Fatalf("clean: %v", err)
	}
	f.st["/data/etc/agent.json"] = Info{Perm: 0o666}
	if r := refusal(t, Check(f, []Target{tgt})); r.Path != "/data/etc/agent.json" {
		t.Errorf("the file behind the middle link: %v", r)
	}
	// An optional path missing below a real link is "not created yet".
	f = newFake("/var", "/data/run")
	link(f, "/var/run", "/data/run")
	if err := Check(f, []Target{{Path: "/var/run/skep", What: "d", Optional: true}}); err != nil {
		t.Errorf("optional below a link: %v", err)
	}
}

func TestCheckRelativeLink(t *testing.T) {
	f := newFake("/a/b", "/a/c/agent.json")
	link(f, "/a/b/agent.json", "../c/agent.json")
	tgt := Target{Path: "/a/b/agent.json", What: "c"}
	if err := Check(f, []Target{tgt}); err != nil {
		t.Fatalf("clean: %v", err)
	}
	f.st["/a/c"] = Info{Perm: 0o777}
	if r := refusal(t, Check(f, []Target{tgt})); r.Path != "/a/c" {
		t.Errorf("relative to the link's directory: %v", r)
	}
	f = newFake("/a/b/agent.json")
	link(f, "/a/l", "b/agent.json")
	f.st["/a/b"] = Info{UID: 5, Perm: 0o755}
	if r := refusal(t, Check(f, []Target{{Path: "/a/l", What: "c"}})); r.Path != "/a/b" {
		t.Errorf("a sibling target: %v", r)
	}
}

func TestCheckLinkLoop(t *testing.T) {
	f := newFake("/a")
	link(f, "/a/x", "/a/y")
	link(f, "/a/y", "x")
	if r := refusal(t, Check(f, []Target{{Path: "/a/x", What: "c", Optional: true}})); !strings.Contains(r.Why, "loop") {
		t.Errorf("loop: %v", r)
	}
}

func TestCheckLinkHopLimit(t *testing.T) {
	chain := func(n int) *fakeFS {
		f := newFake("/l", "/real/agent.json")
		for i := 0; i < n; i++ {
			to := fmt.Sprintf("/l/%d", i+1)
			if i == n-1 {
				to = "/real/agent.json"
			}
			link(f, fmt.Sprintf("/l/%d", i), to)
		}
		return f
	}
	tgt := Target{Path: "/l/0", What: "c"}
	if err := Check(chain(16), []Target{tgt}); err != nil {
		t.Errorf("16 hops are allowed: %v", err)
	}
	if r := refusal(t, Check(chain(17), []Target{tgt})); !strings.Contains(r.Why, "more than 16") {
		t.Errorf("17 hops: %v", r)
	}
}

func TestCheckMissing(t *testing.T) {
	f := newFake("/var/db")
	if err := Check(f, []Target{{Path: "/var/db/skep", What: "d", Optional: true}}); err != nil {
		t.Errorf("an optional directory may be missing: %v", err)
	}
	f.st["/var/db"] = Info{Perm: 0o777}
	if r := refusal(t, Check(f, []Target{{Path: "/var/db/skep", What: "d", Optional: true}})); r.Path != "/var/db" {
		t.Errorf("its parent is still checked: %v", r)
	}
	if r := refusal(t, Check(newFake("/usr"), []Target{bin()})); !strings.Contains(r.Why, "does not exist") {
		t.Errorf("required: %v", r)
	}
	// A dangling link is not "not created yet".
	f = newFake("/var/db")
	f.st["/var/db/skep"] = Info{Symlink: true}
	f.links["/var/db/skep"] = "/gone"
	if r := refusal(t, Check(f, []Target{{Path: "/var/db/skep", What: "d", Optional: true}})); !strings.Contains(r.Why, "cannot be resolved") {
		t.Errorf("dangling link: %v", r)
	}
}

func TestCheckRefusesDotDotAndStatErrors(t *testing.T) {
	f := newFake("/usr/local/etc/agent.json")
	if r := refusal(t, Check(f, []Target{{Path: "/usr/local/x/../etc/agent.json", What: "c"}})); !strings.Contains(r.Why, `".."`) {
		t.Errorf("dotdot: %v", r)
	}
	f.statErr["/usr/local"] = errors.New("I/O error")
	if r := refusal(t, Check(f, []Target{{Path: "/usr/local/etc/agent.json", What: "c"}})); r.Path != "/usr/local" {
		t.Errorf("stat error: %v", r)
	}
}
