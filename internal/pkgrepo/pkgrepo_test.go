package pkgrepo

import (
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
