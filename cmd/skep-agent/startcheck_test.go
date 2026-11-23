package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/greenbeanorg/skep/internal/config"
	"github.com/greenbeanorg/skep/internal/startcheck"
)

func TestProgramTargets(t *testing.T) {
	none := func(string) (string, bool) { return "", false }
	env := func(k string) (string, bool) { return "/env/agent.json", k == config.EnvConfig }
	yes := func(string) bool { return true }
	no := func(string) bool { return false }
	for name, c := range map[string]struct {
		flag   string
		lookup func(string) (string, bool)
		exists func(string) bool
		want   string
	}{
		"flag wins":        {"/f/agent.json", env, yes, "/f/agent.json"},
		"environment":      {"", env, yes, "/env/agent.json"},
		"default exists":   {"", none, yes, config.DefaultConfigPath},
		"no config at all": {"", none, no, ""},
	} {
		got := programTargets("/bin/agent", c.flag, c.lookup, c.exists)
		if got[0].Path != "/bin/agent" || got[0].Optional {
			t.Errorf("%s: binary %+v", name, got[0])
		}
		if c.want == "" {
			if len(got) != 1 {
				t.Errorf("%s: %+v", name, got)
			}
			continue
		}
		if len(got) != 2 || got[1].Path != c.want || got[1].Optional {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestStateTargets(t *testing.T) {
	var cfg config.Config
	cfg.DataDir = "/var/db/skep"
	cfg.Socket.Path = "/var/run/skep/agent.sock"
	cfg.Database.Path = "/var/db/skep/agent/agent.db"
	data := startcheck.Target{Path: "/var/db/skep", What: "the agent's data directory", Optional: true}
	db := startcheck.Target{Path: "/var/db/skep/agent", What: "the agent's database directory", Optional: true}
	sock := startcheck.Target{Path: "/var/run/skep", What: "the agent socket's directory", Optional: true}
	if got := stateTargets(cfg); !slices.Equal(got, []startcheck.Target{data, db, sock}) {
		t.Fatalf("database in its own directory: %+v", got)
	}
	cfg.Database.Path = "/var/db/skep/agent.db"
	if got := stateTargets(cfg); !slices.Equal(got, []startcheck.Target{data, sock}) {
		t.Fatalf("database in the data directory: %+v", got)
	}
}

// The check and the loader must name the same file, from the same resolver.
func TestConfigCheckedIsConfigRead(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(k string) (string, bool) {
		if k == config.EnvConfig {
			return file, true
		}
		return "", false
	}
	checked := configToCheck("", env, func(string) bool { return false })
	_, read, err := config.LoadReportingPath(config.LoadOptions{LookupEnv: env})
	if err != nil {
		t.Fatal(err)
	}
	if checked != file || read != file || checkConfigRead(checked, read) != nil {
		t.Errorf("checked %q, read %q", checked, read)
	}
	none := func(string) (string, bool) { return "", false }
	_, read, err = config.LoadReportingPath(config.LoadOptions{DefaultPath: filepath.Join(dir, "absent.json"), LookupEnv: none})
	if err != nil || read != "" {
		t.Errorf("a missing default file reads nothing: %q, %v", read, err)
	}
}

func TestCheckConfigReadRefusesADifference(t *testing.T) {
	for _, c := range [][2]string{{"/a.json", "/b.json"}, {"", "/usr/local/etc/skep/agent.json"}, {"/a.json", ""}} {
		var r *startcheck.Refusal
		if err := checkConfigRead(c[0], c[1]); !errors.As(err, &r) {
			t.Errorf("checked %q, read %q: %v", c[0], c[1], err)
		}
	}
	if err := checkConfigRead("", ""); err != nil {
		t.Errorf("no file at all: %v", err)
	}
}
