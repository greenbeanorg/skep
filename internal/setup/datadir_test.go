package setup

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
