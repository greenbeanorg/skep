package setup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func writeBoot(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The last file in the loader's order wins.
func TestReadLoaderFollowsTheLoadersOrder(t *testing.T) {
	root := t.TempDir()
	writeBoot(t, root, map[string]string{
		"boot/loader.conf":          "a=\"main\"\nb=\"main\"\nc=\"main\"\nd=\"main\"\nloader_conf_files=\"/boot/extra.conf\"\n",
		"boot/extra.conf":           "b=\"extra\"\nc=\"extra\"\nd=\"extra\"\nloader_conf_files=\"/boot/nested.conf /boot/loader.conf\"\n",
		"boot/nested.conf":          "c=\"nested\"\nd=\"nested\"\n",
		"boot/loader.conf.d/x.conf": "d=\"dir\"\ne=\"dir\"\n",
		"boot/loader.conf.d/x.txt":  "e=\"ignored\"\n",
		"boot/loader.conf.local":    "e=\"local\"\n",
	})
	l, err := readLoader(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"a": "main", "b": "extra", "c": "nested", "d": "dir", "e": "local"} {
		if got, _ := l.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// The loader stats a *.conf entry, following links: a link to a regular
// file is read, and a dangling link, a loop, or a link to a directory is
// skipped.
func TestReadLoaderFollowsSymlinksInTheDirectory(t *testing.T) {
	root := t.TempDir()
	writeBoot(t, root, map[string]string{
		"boot/loader.conf":        "a=\"main\"\n",
		"boot/shared/vm.txt":      "a=\"linked\"\n",
		"boot/loader.conf.d/keep": "",
	})
	dir := filepath.Join(root, "boot/loader.conf.d")
	for link, target := range map[string]string{
		"vm.conf": "../shared/vm.txt", "dangling.conf": "../shared/none", "loop.conf": "loop.conf", "dir.conf": "../shared",
	} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	l, err := readLoader(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := l.Get("a"); got != "linked" {
		t.Errorf("a = %q, want the linked file's value", got)
	}
	if len(l.Overrides) != 1 {
		t.Errorf("%d files read, want only the link to a regular file", len(l.Overrides))
	}
}

func TestReadLoaderHonoursRenamedDirsAndLocal(t *testing.T) {
	root := t.TempDir()
	writeBoot(t, root, map[string]string{
		"boot/loader.conf":          "loader_conf_dirs=\"/boot/mine.d\"\nlocal_loader_conf_files=\"/boot/mine.local\"\n",
		"boot/mine.d/a.conf":        "x=\"dir\"\ny=\"dir\"\n",
		"boot/mine.local":           "y=\"local\"\n",
		"boot/loader.conf.d/z.conf": "x=\"not read\"\n",
		"boot/loader.conf.local":    "y=\"not read\"\n",
	})
	l, err := readLoader(root)
	if err != nil {
		t.Fatal(err)
	}
	if x, _ := l.Get("x"); x != "dir" {
		t.Errorf("x = %q", x)
	}
	if y, _ := l.Get("y"); y != "local" {
		t.Errorf("y = %q", y)
	}
}

func TestReadLoaderSkipsWhatItMustNotOpen(t *testing.T) {
	root := t.TempDir()
	writeBoot(t, root, map[string]string{"boot/loader.conf": "loader_conf_files=\"/boot/fifo /boot/absent ../../boot/rel.conf\"\n", "boot/rel.conf": "r=\"1\"\n"})
	if err := syscall.Mkfifo(filepath.Join(root, "boot/fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := readLoader(root)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := l.Get("r"); r != "1" {
		t.Errorf("a path with .. did not stay under the root: r = %q", r)
	}
}

func TestParseKenvNames(t *testing.T) {
	got := parseKenvNames("hw.vmm.a=\"0\"\nkern.b=\"1\"\nnot a name=\"x\"\n\n")
	if !slices.Equal(got, []string{"hw.vmm.a", "kern.b"}) {
		t.Fatalf("%v", got)
	}
}

func TestStaleVMMTunables(t *testing.T) {
	loader := RCConf{Main: ShVars{Vals: map[string]string{"hw.vmm.kept": "1"}},
		Overrides: []ShVars{{Vals: map[string]string{"hw.vmm.over": "1"}}}}
	defaults := RCConf{Main: ShVars{Vals: map[string]string{"hw.vmm.default": "1"}}}
	got := staleVMMTunables([]string{"hw.vmm.kept", "hw.vmm.over", "hw.vmm.default", "hw.vmm.gone", "kern.other"}, loader, defaults)
	if !slices.Equal(got, []string{"hw.vmm.gone"}) {
		t.Fatalf("%v", got)
	}
}

func TestPlanWarnsAboutAStaleTunableAndChangesNoAction(t *testing.T) {
	root := stockHost(t)
	o := Options{Root: root, Mgmt: []string{"vtnet0"}}
	before := actions(compute(t, o, facts()))
	f := facts()
	f.KenvNames = func() []string { return []string{"hw.vmm.vmx.cap.virtual_interrupt_delivery", "kernel"} }
	p := compute(t, o, f)
	if !slices.Equal(actions(p), before) {
		t.Fatalf("actions changed: %v", actions(p))
	}
	found := false
	for _, it := range p.Items {
		if it.Kind == Warn && strings.Contains(it.What, "hw.vmm.vmx.cap.virtual_interrupt_delivery") {
			found = true
		}
	}
	if !found {
		t.Fatal("no warning")
	}
}
