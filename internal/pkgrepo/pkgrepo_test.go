package pkgrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// The stock /etc/pkg/FreeBSD.conf of FreeBSD 15.1 (smith and langrishe,
// 2026-11-22), without the url and key lines.
const stock = `# $FreeBSD$
FreeBSD-ports: {
  url: "pkg+https://pkg.FreeBSD.org/${ABI}/quarterly",
  enabled: yes
}
FreeBSD-base: {
  url: "pkg+https://pkg.FreeBSD.org/${ABI}/base_release_${VERSION_MINOR}",
  enabled: no
}
`

func TestBase(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  bool
		where string
	}{
		{"nothing", nil, false, ""},
		{"stock (smith)", map[string]string{"etc/pkg/FreeBSD.conf": stock}, false, "/etc/pkg/FreeBSD.conf"},
		{"stock plus override (langrishe)", map[string]string{"etc/pkg/FreeBSD.conf": stock,
			"usr/local/etc/pkg/repos/FreeBSD.conf": "FreeBSD-base: { enabled: yes }\n"}, true, "/usr/local/etc/pkg/repos/FreeBSD.conf"},
		{"override that disables", map[string]string{"etc/pkg/FreeBSD.conf": "FreeBSD-base: { enabled: yes }",
			"usr/local/etc/pkg/repos/a.conf": "FreeBSD-base: { enabled: false }"}, false, "/usr/local/etc/pkg/repos/a.conf"},
		{"override without enabled keeps it", map[string]string{"etc/pkg/FreeBSD.conf": "FreeBSD-base: { enabled: yes }",
			"usr/local/etc/pkg/repos/mirror.conf": "FreeBSD-base: { url: \"pkg+https://mirror/x\" }"}, true, "/etc/pkg/FreeBSD.conf"},
		{"first definition without enabled is enabled", map[string]string{"usr/local/etc/pkg/repos/b.conf": "\"FreeBSD-base\": { url: \"x\" }"}, true, "/usr/local/etc/pkg/repos/b.conf"},
		{"name order within a directory", map[string]string{"usr/local/etc/pkg/repos/a.conf": "FreeBSD-base: { enabled: yes }",
			"usr/local/etc/pkg/repos/b.conf": "FreeBSD-base: { enabled: no }"}, false, "/usr/local/etc/pkg/repos/b.conf"},
		{"commented out", map[string]string{"etc/pkg/FreeBSD.conf": stock,
			"usr/local/etc/pkg/repos/FreeBSD.conf": "#FreeBSD-base: { enabled: yes }\n"}, false, "/etc/pkg/FreeBSD.conf"},
		{"not .conf", map[string]string{"etc/pkg/FreeBSD.conf": stock,
			"usr/local/etc/pkg/repos/FreeBSD.conf.sample": "FreeBSD-base: { enabled: yes }"}, false, "/etc/pkg/FreeBSD.conf"},
		{"block without a colon", map[string]string{"etc/pkg/FreeBSD.conf": stock,
			"usr/local/etc/pkg/repos/FreeBSD.conf": "FreeBSD-base {\n  enabled = yes\n}\n"}, true, "/usr/local/etc/pkg/repos/FreeBSD.conf"},
		{"block with =", map[string]string{"etc/pkg/FreeBSD.conf": stock,
			"usr/local/etc/pkg/repos/FreeBSD.conf": "FreeBSD-base = { enabled: true }"}, true, "/usr/local/etc/pkg/repos/FreeBSD.conf"},
		{"enabled without a separator", map[string]string{"etc/pkg/FreeBSD.conf": stock,
			"usr/local/etc/pkg/repos/FreeBSD.conf": "FreeBSD-base { enabled yes }"}, true, "/usr/local/etc/pkg/repos/FreeBSD.conf"},
		{"block comment", map[string]string{"etc/pkg/FreeBSD.conf": stock,
			"usr/local/etc/pkg/repos/FreeBSD.conf": "/* FreeBSD-base: {\n enabled: yes\n} */\n"}, false, "/etc/pkg/FreeBSD.conf"},
		{"line comment", map[string]string{"etc/pkg/FreeBSD.conf": stock,
			"usr/local/etc/pkg/repos/FreeBSD.conf": "// FreeBSD-base: { enabled: yes }\n"}, false, "/etc/pkg/FreeBSD.conf"},
		{"url with // on one line", map[string]string{"etc/pkg/FreeBSD.conf": stock,
			"usr/local/etc/pkg/repos/FreeBSD.conf": "FreeBSD-base: { url: \"pkg+https://mirror/x\", enabled: yes } // mirror\n"}, true, "/usr/local/etc/pkg/repos/FreeBSD.conf"},
		{"other repository", map[string]string{"etc/pkg/FreeBSD.conf": stock,
			"usr/local/etc/pkg/repos/x.conf": "FreeBSD-base-latest: { enabled: yes }"}, false, "/etc/pkg/FreeBSD.conf"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			for p, s := range c.files {
				fsys[p] = &fstest.MapFile{Data: []byte(s)}
			}
			got, where := Base(fsys)
			if got != c.want || where != c.where {
				t.Fatalf("Base = %v, %q; want %v, %q", got, where, c.want, c.where)
			}
		})
	}
}

// TestEnableBase runs the printed command, as written but with its
// /usr/local moved under a temporary root, on a host that has only the
// stock /etc/pkg/FreeBSD.conf and no /usr/local/etc/pkg (a host installed
// from the distribution sets, like smith): afterwards Base says enabled.
// Run twice, it stays enabled.
func TestEnableBase(t *testing.T) {
	if !strings.HasPrefix(EnableBase, "mkdir -p /usr/local/etc/pkg/repos && ") {
		t.Fatalf("EnableBase %q does not create the directory first", EnableBase)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc/pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/pkg/FreeBSD.conf"), []byte(stock), 0o644); err != nil {
		t.Fatal(err)
	}
	if on, _ := Base(os.DirFS(root)); on {
		t.Fatal("enabled before")
	}
	cmd := strings.ReplaceAll(EnableBase, "/usr/local/", root+"/usr/local/")
	for i := 0; i < 2; i++ {
		if out, err := exec.Command(sh, "-c", cmd).CombinedOutput(); err != nil {
			t.Fatalf("run %d: %v: %s", i, err, out)
		}
		if on, where := Base(os.DirFS(root)); !on || where != "/usr/local/etc/pkg/repos/FreeBSD.conf" {
			t.Fatalf("run %d: enabled %v by %q", i, on, where)
		}
	}
}
