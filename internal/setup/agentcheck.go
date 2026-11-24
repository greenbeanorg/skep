package setup

import (
	"errors"

	"github.com/greenbeanorg/skep/internal/config"
	"github.com/greenbeanorg/skep/internal/startcheck"
)

// AgentStartReport is the agent's own start check, run by skep-setup check
// on the paths the rc.d script would start the agent with.
type AgentStartReport struct {
	Paths   []string            // checked, each with its parents up to /
	Refusal *startcheck.Refusal // the first refusal, where the agent would stop
	Problem string              // why the check could not run
	// ArgsIgnored: skep_agent_args is set; it could change the paths and
	// the check does not read it.
	ArgsIgnored bool
}

// CheckAgentStart runs the agent's start check, in the agent's order: the
// program's paths before its configuration is read. It only reads.
func CheckAgentStart(root string, fsys startcheck.FS) (r AgentStartReport) {
	rc, err := ReadRCConf(root)
	if err != nil {
		r.Problem = "rc.conf could not be read: " + err.Error()
		return r
	}
	args, _ := rc.Get("skep_agent_args")
	r.ArgsIgnored = args != ""
	bin, _ := rc.Get("skep_agent_bin")
	if bin == "" {
		bin = agentBin // the rc.d script's default
	}
	file, _ := rc.Get("skep_agent_config")
	configPath := config.FileToCheck(file, noEnv, func(p string) bool {
		return config.FileMayExist(hostPath(root, p))
	})
	targets := startcheck.ProgramTargets(bin, configPath)
	if r.run(fsys, targets) {
		return r
	}
	opts, err := agentLoadOptions(root)
	if err != nil {
		r.Problem = "rc.conf could not be read: " + err.Error()
		return r
	}
	cfg, err := config.Load(opts)
	if err != nil {
		r.Problem = "the agent's configuration could not be loaded: " + err.Error()
		return r
	}
	r.run(fsys, append(targets, startcheck.StateTargets(cfg.DataDir, cfg.Database.Path, cfg.Socket.Path)...))
	return r
}

// run checks targets, records them, and reports whether the check refused.
func (r *AgentStartReport) run(fsys startcheck.FS, targets []startcheck.Target) bool {
	r.Paths = r.Paths[:0]
	for _, t := range targets {
		r.Paths = append(r.Paths, t.Path)
	}
	err := startcheck.Check(fsys, targets)
	if err != nil && !errors.As(err, &r.Refusal) {
		r.Problem = err.Error()
	}
	return err != nil
}
