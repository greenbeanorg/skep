package main

import (
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
	got := stateTargets(cfg)
	want := []startcheck.Target{{Path: "/var/db/skep", What: "the agent's data directory", Optional: true},
		{Path: "/var/run/skep", What: "the agent socket's directory", Optional: true}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("%+v", got)
	}
}
