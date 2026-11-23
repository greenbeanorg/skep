package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/greenbeanorg/skep/internal/config"
	"github.com/greenbeanorg/skep/internal/startcheck"
)

// programTargets are the running binary and the configuration file the
// agent is about to read (ADR 0004, amendment of 2026-11-23): -config, or
// the environment's path, or the default file when it exists.
func programTargets(exe, configFlag string, lookup func(string) (string, bool), exists func(string) bool) []startcheck.Target {
	out := []startcheck.Target{{Path: exe, What: "the agent's binary"}}
	path := configFlag
	if path == "" {
		if v, ok := lookup(config.EnvConfig); ok && v != "" {
			path = v
		} else if exists(config.DefaultConfigPath) {
			path = config.DefaultConfigPath
		}
	}
	if path != "" {
		out = append(out, startcheck.Target{Path: path, What: "the agent's configuration"})
	}
	return out
}

// stateTargets are the data directory and the socket's directory. Either
// may not exist yet; the parents that do are checked.
func stateTargets(cfg config.Config) []startcheck.Target {
	return []startcheck.Target{
		{Path: cfg.DataDir, What: "the agent's data directory", Optional: true},
		{Path: filepath.Dir(cfg.Socket.Path), What: "the agent socket's directory", Optional: true},
	}
}

// checkProgram runs before the configuration is read.
func checkProgram(configFlag string) error {
	fsys, ok := startFS()
	if !ok {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return &startcheck.Refusal{Path: "the running program", Why: "cannot be located: " + err.Error()}
	}
	return startcheck.Check(fsys, programTargets(exe, configFlag, os.LookupEnv, func(p string) bool {
		_, err := os.Lstat(p)
		return !errors.Is(err, fs.ErrNotExist)
	}))
}

// checkState runs after the configuration is read and before the database
// is opened or the socket created.
func checkState(cfg config.Config) error {
	fsys, ok := startFS()
	if !ok {
		return nil
	}
	return startcheck.Check(fsys, stateTargets(cfg))
}
