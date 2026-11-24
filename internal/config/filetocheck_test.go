package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileToCheck(t *testing.T) {
	cases := []struct {
		name, flag, env string
		exists          bool
		want            string
	}{
		{"flag wins over env", "/f.json", "/e.json", false, "/f.json"},
		{"flag need not exist", "/f.json", "", false, "/f.json"},
		{"env need not exist", "", "/e.json", false, "/e.json"},
		{"default present", "", "", true, DefaultConfigPath},
		{"default absent", "", "", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lookup := func(k string) (string, bool) {
				return c.env, k == EnvConfig && c.env != ""
			}
			got := FileToCheck(c.flag, lookup, func(string) bool { return c.exists })
			if got != c.want {
				t.Fatalf("FileToCheck = %q, want %q", got, c.want)
			}
		})
	}
}

// A path that cannot be examined must still be checked, so only a definite
// "not there" counts as absent.
func TestFileMayExist(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		file:                         true,
		filepath.Join(dir, "absent"): false,
		filepath.Join(file, "below"): true, // ENOTDIR is not "not there"
	} {
		if got := FileMayExist(path); got != want {
			t.Errorf("FileMayExist(%s) = %v, want %v", path, got, want)
		}
	}
}
