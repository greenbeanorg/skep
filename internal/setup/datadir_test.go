package setup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/internal/startcheck"
)

// mkSetup makes /var/db/skep/setup with exactly mode perm.
func mkSetup(root string, perm fs.FileMode) {
	p := filepath.Join(root, "var/db/skep/setup")
	os.Mkdir(p, 0o700)
	os.Chmod(p, perm)
}

func TestDataDirRefusalType(t *testing.T) {
	var r *DataDirRefusal
	if err := asRefusal(errors.New("refused")); !errors.As(err, &r) {
		t.Fatalf("%T is not a DataDirRefusal", err)
	}
	if err := asRefusal(fmt.Errorf("/var/db/skep/setup: %w", fs.ErrPermission)); errors.As(err, &r) || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("a permission error became %T", err)
	}
	if err := asRefusal(nil); err != nil {
		t.Fatal(err)
	}
}

func TestCheckDataDir(t *testing.T) {
	asRoot := func(fs.FileInfo) (uint32, bool) { return 0, true }
	mk := func(t *testing.T) string {
		root := t.TempDir()
		for _, d := range []string{"var/db/skep"} {
			if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		for _, d := range []string{"var", "var/db", "var/db/skep"} {
			os.Chmod(filepath.Join(root, d), 0o755)
		}
		return root
	}
	if err := checkDataDir(mk(t), asRoot); err != nil {
		t.Fatalf("a clean layout: %v", err)
	}
	root := mk(t)
	os.Remove(filepath.Join(root, "var/db/skep"))
	if err := checkDataDir(root, asRoot); err != nil {
		t.Fatalf("no /var/db/skep yet: %v", err)
	}
	clean := mk(t)
	mkSetup(clean, 0o700)
	if err := checkDataDir(clean, asRoot); err != nil {
		t.Fatalf("a clean setup directory: %v", err)
	}
	cases := []struct {
		name, want string
		change     func(root string)
		uid        func(fs.FileInfo) (uint32, bool)
	}{
		{"skep group-writable", "/var/db/skep is writable by its group or others (0775)", func(r string) { os.Chmod(filepath.Join(r, "var/db/skep"), 0o775) }, asRoot},
		{"/var/db other-writable", "/var/db is writable", func(r string) { os.Chmod(filepath.Join(r, "var/db"), 0o757) }, asRoot},
		{"not root's", "/var/db/skep is owned by UID 409, not root", nil, func(st fs.FileInfo) (uint32, bool) {
			if st.Name() == "skep" {
				return 409, true
			}
			return 0, true
		}},
		{"a symlink", "/var/db/skep is a symbolic link", func(r string) {
			os.Remove(filepath.Join(r, "var/db/skep"))
			os.Mkdir(filepath.Join(r, "elsewhere"), 0o755)
			os.Symlink(filepath.Join(r, "elsewhere"), filepath.Join(r, "var/db/skep"))
		}, asRoot},
		{"a file", "/var/db/skep is not a directory", func(r string) {
			os.Remove(filepath.Join(r, "var/db/skep"))
			os.WriteFile(filepath.Join(r, "var/db/skep"), nil, 0o644)
		}, asRoot},
		{"no /var/db", "/var/db:", func(r string) { os.RemoveAll(filepath.Join(r, "var/db")) }, asRoot},
		{"setup group-writable", "/var/db/skep/setup is writable by its group or others (0770)", func(r string) { mkSetup(r, 0o770) }, asRoot},
		{"setup other-writable", "/var/db/skep/setup is writable by its group or others (0702)", func(r string) { mkSetup(r, 0o702) }, asRoot},
		{"setup not root's", "/var/db/skep/setup is owned by UID 409, not root", func(r string) { mkSetup(r, 0o700) }, func(st fs.FileInfo) (uint32, bool) {
			if st.Name() == "setup" {
				return 409, true
			}
			return 0, true
		}},
		{"setup a symlink", "/var/db/skep/setup is a symbolic link", func(r string) {
			os.Mkdir(filepath.Join(r, "elsewhere"), 0o700)
			os.Symlink(filepath.Join(r, "elsewhere"), filepath.Join(r, "var/db/skep/setup"))
		}, asRoot},
		{"setup a file", "/var/db/skep/setup is not a directory", func(r string) {
			os.WriteFile(filepath.Join(r, "var/db/skep/setup"), nil, 0o600)
		}, asRoot},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := mk(t)
			if c.change != nil {
				c.change(root)
			}
			err := checkDataDir(root, c.uid)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
		})
	}
	if err := CheckDataDir(t.TempDir()); err != nil {
		t.Fatalf("a test root is not checked: %v", err)
	}
}

// aclFS is a host of clean root-owned directories, with one path's ACL or
// owner replaced.
type aclFS struct {
	path, acl string
	uid       uint32
}

func (f aclFS) Lstat(p string) (startcheck.Info, error) {
	st := startcheck.Info{Perm: 0o755}
	if p == f.path {
		st.UID = f.uid
	}
	return st, nil
}
func (aclFS) Readlink(string) (string, error) { return "", fs.ErrInvalid }
func (f aclFS) ACL(p string) (string, error) {
	if p == f.path && f.acl != "" {
		return f.acl, nil
	}
	return "user::rwx\ngroup::r-x\nother::r-x\n", nil
}

func TestDataDirRulesMatchTheStartCheck(t *testing.T) {
	if err := checkDataDirRules(aclFS{}); err != nil {
		t.Fatalf("a clean layout: %v", err)
	}
	cases := []struct {
		name, want string
		fsys       aclFS
	}{
		{"POSIX ACL on the data directory", "/var/db/skep has an ACL beyond its mode bits",
			aclFS{path: "/var/db/skep", acl: "user::rwx\nuser:1001:rwx\ngroup::r-x\nmask::rwx\nother::r-x\n"}},
		{"NFSv4 ACL on a parent", "/var/db has an ACL beyond its mode bits",
			aclFS{path: "/var/db", acl: "user:1001:rwxpDdaARWcCos:-------:allow\nowner@:rwxpDdaARWcCos:-------:allow\ngroup@:r-x---a-R-c--s:-------:allow\neveryone@:r-x---a-R-c--s:-------:allow\n"}},
		{"ACL on setup", "/var/db/skep/setup has an ACL beyond its mode bits",
			aclFS{path: "/var/db/skep/setup", acl: "user::rwx\nuser:1001:rwx\ngroup::---\nmask::rwx\nother::---\n"}},
		{"unknown ACL output", "does not understand", aclFS{path: "/var", acl: "garbage\n"}},
		{"owner of /", "/ is owned by UID 409", aclFS{path: "/", uid: 409}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkDataDirRules(c.fsys)
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "setup will not use") {
				t.Fatalf("err %v, want %q", err, c.want)
			}
			var r *DataDirRefusal
			if !errors.As(asRefusal(err), &r) {
				t.Fatalf("%T is not a refusal (exit 3)", err)
			}
		})
	}
}
