package main

import (
	"errors"
	"io/fs"
	"os"

	"github.com/greenbeanorg/skep/internal/config"
	"github.com/greenbeanorg/skep/internal/startcheck"
)

// configToCheck is the file config.Load will read, from the same resolver:
// empty when no path is given and the default file does not exist.
func configToCheck(configFlag string, lookup func(string) (string, bool), exists func(string) bool) string {
	path, explicit := config.ResolvePath(configFlag, "", lookup)
	if !explicit && !exists(path) {
		return ""
	}
	return path
}

// programTargets are the running binary and the configuration file the
// agent is about to read (ADR 0004, amendment of 2026-11-23).
func programTargets(exe, configFlag string, lookup func(string) (string, bool), exists func(string) bool) []startcheck.Target {
	return startcheck.ProgramTargets(exe, configToCheck(configFlag, lookup, exists))
}

// checkConfigRead refuses when the file Load read is not the one checked,
// for example a default file that appeared after the check.
func checkConfigRead(checked, read string) error {
	if checked == read {
		return nil
	}
	shown := func(p string) string {
		if p == "" {
			return "no file"
		}
		return p
	}
	return &startcheck.Refusal{Path: shown(read), Why: "is what the configuration loader read, but the start check examined " + shown(checked)}
}

// stateTargets are the data directory, the directory holding the database
// when it is another one, and the socket's directory. Any may not exist
// yet; the parents that do are checked.
func stateTargets(cfg config.Config) []startcheck.Target {
	return startcheck.StateTargets(cfg.DataDir, cfg.Database.Path, cfg.Socket.Path)
}

// checkProgram runs before the configuration is read. It returns the
// configuration path it checked, and whether a check ran at all.
func checkProgram(configFlag string) (checked string, active bool, err error) {
	fsys, ok := startFS()
	if !ok {
		return "", false, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", true, &startcheck.Refusal{Path: "the running program", Why: "cannot be located: " + err.Error()}
	}
	exists := func(p string) bool {
		_, err := os.Lstat(p)
		return !errors.Is(err, fs.ErrNotExist)
	}
	checked = configToCheck(configFlag, os.LookupEnv, exists)
	return checked, true, startcheck.Check(fsys, programTargets(exe, configFlag, os.LookupEnv, exists))
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
