package hostops

import (
	"strings"
	"testing"
	"testing/fstest"
)

// TestPkgbaseAvailable: pkg installed is not enough; FreeBSD-base must be
// enabled in pkg's repository files, as skep-setup's check says (smith,
// 2026-11-22: the agent said available, pkg found no FreeBSD-runtime).
func TestPkgbaseAvailable(t *testing.T) {
	stock := "FreeBSD-ports: { enabled: yes }\nFreeBSD-base: {\n  url: \"pkg+https://pkg.FreeBSD.org/${ABI}/base_release_${VERSION_MINOR}\",\n  enabled: no\n}\n"
	h := testHost(t, &script{}, true)
	sys := h.sys.(fakeSystem)
	if ok, why := h.PkgbaseAvailable(); ok || !strings.Contains(why, pkgBootstrap+" is not installed") {
		t.Fatalf("no pkg: %v %q", ok, why)
	}
	sys.paths[pkgBootstrap], sys.paths[pkgBinary] = true, true

	h.rootFS = fstest.MapFS{"etc/pkg/FreeBSD.conf": {Data: []byte(stock)}}
	if ok, why := h.PkgbaseAvailable(); ok || !strings.Contains(why, "FreeBSD-base repository is not enabled (/etc/pkg/FreeBSD.conf)") {
		t.Fatalf("stock (smith): %v %q", ok, why)
	}
	h.rootFS = fstest.MapFS{}
	if ok, why := h.PkgbaseAvailable(); ok || !strings.Contains(why, "no repository file defines it") {
		t.Fatalf("no files: %v %q", ok, why)
	}
	h.rootFS = fstest.MapFS{"etc/pkg/FreeBSD.conf": {Data: []byte(stock)},
		"usr/local/etc/pkg/repos/FreeBSD.conf": {Data: []byte("FreeBSD-base: { enabled: yes }\n")}}
	if ok, why := h.PkgbaseAvailable(); !ok {
		t.Fatalf("override (langrishe): %q", why)
	}
}
