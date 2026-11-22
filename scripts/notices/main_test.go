package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLicenseFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "LICENSE"), "MIT\r\nline\r\n\r\n")
	write(t, filepath.Join(dir, "license-3RD-PARTY.md"), "third")
	write(t, filepath.Join(dir, "PATENTS"), "patents")
	write(t, filepath.Join(dir, "README.md"), "not a license")
	write(t, filepath.Join(dir, "LICENSEE"), "not a license either")
	write(t, filepath.Join(dir, "sub", "LICENSE"), "not at the root")
	files, err := licenseFiles(dir, "x")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	if got := strings.Join(names, " "); got != "LICENSE PATENTS license-3RD-PARTY.md" {
		t.Fatalf("files = %s", got)
	}
	if files[0].Text != "MIT\nline\n" {
		t.Fatalf("text not normalised: %q", files[0].Text)
	}
	if _, err := licenseFiles(filepath.Join(dir, "sub", "none"), "x"); err == nil {
		t.Fatal("a missing directory was accepted")
	}
	empty := filepath.Join(dir, "empty")
	write(t, filepath.Join(empty, "index.js"), "")
	if _, err := licenseFiles(empty, "x"); err == nil || !strings.Contains(err.Error(), "no license file") {
		t.Fatalf("a component without a license file: %v", err)
	}
}

func TestNPMPackages(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	write(t, filepath.Join("scripts", "notices", "vendored", "shadcn-ui", "LICENSE.md"), "shadcn MIT")
	web := filepath.Join(root, "web")
	write(t, filepath.Join(web, "package-lock.json"), `{"lockfileVersion": 3, "packages": {
		"": {"name": "skep-web"},
		"node_modules/react": {"version": "19.0.0"},
		"node_modules/@scope/lib": {"version": "1.0.0"},
		"node_modules/@scope/lib/node_modules/inner": {"version": "0.1.0"},
		"node_modules/vitest": {"version": "5.0.0", "dev": true},
		"node_modules/@types/react": {"version": "19.0.0", "devOptional": true},
		"node_modules/tailwindcss": {"version": "4.0.0", "dev": true},
		"node_modules/vite": {"version": "7.0.0", "dev": true},
		"node_modules/linked": {"link": true}
	}}`)
	for _, p := range []string{"react", "@scope/lib", "@scope/lib/node_modules/inner", "tailwindcss", "vite"} {
		write(t, filepath.Join(web, "node_modules", p, "LICENSE"), p+" license")
	}
	ns, err := npmPackages("web")
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, n := range ns {
		titles = append(titles, n.Title())
	}
	want := []string{
		"@scope/lib 1.0.0",
		"inner 0.1.0 (nested in @scope/lib)",
		"react 19.0.0",
		"tailwindcss 4.0.0 (build tool; its generated code is in the bundle)",
		"vite 7.0.0 (build tool; its generated code is in the bundle)",
		"shadcn/ui (components copied into web/src/components/ui)",
	}
	if strings.Join(titles, "\n") != strings.Join(want, "\n") {
		t.Fatalf("packages:\n%s\nwant:\n%s", strings.Join(titles, "\n"), strings.Join(want, "\n"))
	}
	// A runtime package missing from node_modules is an error, not a gap.
	if err := os.RemoveAll(filepath.Join(web, "node_modules", "react")); err != nil {
		t.Fatal(err)
	}
	if _, err := npmPackages("web"); err == nil || !strings.Contains(err.Error(), "make web-install") {
		t.Fatalf("a missing package: %v", err)
	}
}
