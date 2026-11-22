package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is the repository, two levels up from this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	r, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// fakeELF writes the 64-byte header of an ELF64 executable for the given
// OS ABI and machine; debug/elf accepts it with no sections.
func fakeELF(t *testing.T, path string, osabi byte, machine uint16) {
	t.Helper()
	h := make([]byte, 64)
	copy(h, "\x7fELF")
	h[4], h[5], h[6], h[7] = 2, 1, 1, osabi  // 64-bit, little-endian, version 1
	binary.LittleEndian.PutUint16(h[16:], 2) // ET_EXEC
	binary.LittleEndian.PutUint16(h[18:], machine)
	binary.LittleEndian.PutUint32(h[20:], 1)
	binary.LittleEndian.PutUint16(h[52:], 64) // e_ehsize
	if err := os.WriteFile(path, h, 0o755); err != nil {
		t.Fatal(err)
	}
}

func fakeBins(t *testing.T, osabi byte) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range []string{"skep-agent", "skep-api", "skep-setup", "skepctl"} {
		fakeELF(t, filepath.Join(dir, n), osabi, 62)
	}
	return dir
}

func TestStageLayoutManifestPlist(t *testing.T) {
	out := filepath.Join(t.TempDir(), "pkg")
	r, err := Stage(Options{Repo: repoRoot(t), BinDir: fakeBins(t, 9), Out: out, Version: "0.1.0", CheckELF: true})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]os.FileMode{
		"usr/local/sbin/skep-agent":                         0o555,
		"usr/local/sbin/skep-api":                           0o555,
		"usr/local/sbin/skep-setup":                         0o555,
		"usr/local/bin/skepctl":                             0o555,
		"usr/local/etc/rc.d/skep_agent":                     0o555,
		"usr/local/etc/rc.d/skep_api":                       0o555,
		"usr/local/etc/skep/agent.json.sample":              0o644,
		"usr/local/etc/skep/api.json.sample":                0o644,
		"usr/local/share/licenses/skep/LICENSE":             0o444,
		"usr/local/share/licenses/skep/THIRD_PARTY_NOTICES": 0o444,
	}
	var got []string
	err = filepath.Walk(r.Stage, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(r.Stage, p)
		got = append(got, rel)
		if m, ok := want[rel]; !ok {
			t.Errorf("unexpected file in the package: %s", rel)
		} else if info.Mode().Perm() != m {
			t.Errorf("%s mode %o, want %o", rel, info.Mode().Perm(), m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Errorf("staged %d files, want %d: %v", len(got), len(want), got)
	}
	// Never the configuration itself (Decision 1).
	for _, f := range got {
		if strings.HasSuffix(f, "agent.json") || strings.HasSuffix(f, "api.json") {
			t.Errorf("the package must not contain %s", f)
		}
	}

	var m manifest
	b, err := os.ReadFile(r.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("+MANIFEST is not JSON (UCL subset): %v", err)
	}
	if m.Name != "skep" || m.Origin != "sysutils/skep" || m.ABI != "FreeBSD:15:amd64" || m.Prefix != "/usr/local" ||
		m.Version != "0.1.0" || len(m.Licenses) != 1 || m.Licenses[0] != "BSD2CLAUSE" || m.LicenseLogic != "single" {
		t.Errorf("manifest = %+v", m)
	}
	if len(m.Scripts) != 2 || !strings.Contains(m.Scripts["pre-install"], "409") ||
		!strings.Contains(m.Scripts["pre-deinstall"], "PKG_UPGRADE") {
		t.Errorf("manifest scripts = %v", keys(m.Scripts))
	}
	if strings.Contains(string(b), "@") && strings.Contains(string(b), "gmail") {
		t.Error("no personal address in the manifest")
	}

	plist, _ := os.ReadFile(r.Plist)
	for _, line := range []string{
		"@(root,wheel,0555) sbin/skep-agent",
		"@(root,wheel,0555) sbin/skep-setup",
		"@(root,wheel,0555) bin/skepctl",
		"@(root,wheel,0644) etc/skep/agent.json.sample",
		"@(root,wheel,0444) share/licenses/skep/THIRD_PARTY_NOTICES",
	} {
		if !bytes.Contains(plist, []byte(line+"\n")) {
			t.Errorf("plist lacks %q:\n%s", line, plist)
		}
	}
	if bytes.Contains(plist, []byte("@sample")) {
		t.Error("@sample would install agent.json from the sample; setup writes it")
	}
	sums, _ := os.ReadFile(r.Sums)
	if n := bytes.Count(sums, []byte("\n")); n != len(want) {
		t.Errorf("SHA256SUMS.stage has %d lines, want %d", n, len(want))
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestStageRefusesLinuxBinaries(t *testing.T) {
	_, err := Stage(Options{Repo: repoRoot(t), BinDir: fakeBins(t, 0), Out: filepath.Join(t.TempDir(), "pkg"), Version: "0.1.0", CheckELF: true})
	if err == nil || !strings.Contains(err.Error(), "FreeBSD amd64") {
		t.Fatalf("Linux binaries staged: %v", err)
	}
}

func TestStageRefusesForeignOutputDir(t *testing.T) {
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Stage(Options{Repo: repoRoot(t), BinDir: fakeBins(t, 9), Out: out, Version: "0.1.0"})
	if err == nil || !strings.Contains(err.Error(), "not made by pkgstage") {
		t.Fatalf("a foreign directory was used: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "keep")); err != nil {
		t.Fatal("a file in a foreign output directory was removed")
	}
	for _, bad := range []string{"", "/"} {
		if _, err := Stage(Options{Repo: repoRoot(t), BinDir: fakeBins(t, 9), Out: bad, Version: "0.1.0"}); err == nil {
			t.Errorf("output %q accepted", bad)
		}
	}
}

func TestStageReplacesItsOwnOutput(t *testing.T) {
	out := filepath.Join(t.TempDir(), "pkg")
	o := Options{Repo: repoRoot(t), BinDir: fakeBins(t, 9), Out: out, Version: "0.1.0"}
	if _, err := Stage(o); err != nil {
		t.Fatal(err)
	}
	if _, err := Stage(o); err != nil {
		t.Fatalf("second run: %v", err)
	}
}

func TestStageRefusesBadVersionAndMissingBinary(t *testing.T) {
	if _, err := Stage(Options{Repo: repoRoot(t), BinDir: fakeBins(t, 9), Out: filepath.Join(t.TempDir(), "p"), Version: "0.1.0; rm"}); err == nil {
		t.Error("bad version accepted")
	}
	bins := fakeBins(t, 9)
	os.Remove(filepath.Join(bins, "skep-setup"))
	if _, err := Stage(Options{Repo: repoRoot(t), BinDir: bins, Out: filepath.Join(t.TempDir(), "p"), Version: "0.1.0"}); err == nil {
		t.Error("staged without skep-setup")
	}
}
