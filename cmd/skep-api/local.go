package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"syscall"

	"github.com/greenbeanorg/skep/internal/config"
)

// Local commands are run on the host by its administrator: "skep-api tls
// ..." and "skep-api admin ...". They have no network path. They open
// skep-api's own state directly, so they must run as the service account:
// started as root they give up root first, and as any other user they fail
// on file permissions.

// localFlags are the flags every local command accepts.
type localFlags struct {
	set        *flag.FlagSet
	configPath *string
	dataDir    *string
	dev        *bool
}

func newLocalFlags(name string, stderr io.Writer) *localFlags {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(stderr)
	return &localFlags{
		set:        set,
		configPath: set.String("config", "", "path to the JSON configuration file (default "+config.DefaultAPIConfigPath+" if it exists)"),
		dataDir:    set.String("data-dir", "", "state directory"),
		dev:        set.Bool("dev", false, "development mode: use the local data directory"),
	}
}

// prepare parses args, loads the configuration, and, when the command was
// started as root, drops to the account that owns skep-api's state
// directory before anything is opened.
func (f *localFlags) prepare(args []string, e env) (config.API, error) {
	if err := f.set.Parse(args); err != nil {
		return config.API{}, err
	}
	if f.set.NArg() > 0 {
		return config.API{}, fmt.Errorf("unexpected argument %q", f.set.Arg(0))
	}
	overrides := config.APIOverrides{DataDir: *f.dataDir}
	f.set.Visit(func(fl *flag.Flag) {
		if fl.Name == "dev" {
			overrides.DevMode = f.dev
		}
	})
	cfg, err := config.LoadAPI(config.APILoadOptions{Path: *f.configPath, Overrides: overrides})
	if err != nil {
		return config.API{}, err
	}
	if e.euid == 0 {
		if err := dropToStateOwner(cfg.StateDir(), e); err != nil {
			return config.API{}, err
		}
	}
	return cfg, nil
}

// dropToStateOwner gives up root for the owner of skep-api's state
// directory, which is the account the service runs as.
func dropToStateOwner(dir string, e env) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s does not exist yet: start the skep_api service once, which creates it for the "+
			"service account, and run this command again", dir)
	}
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if st.Uid == 0 {
		return fmt.Errorf("%s is owned by root; it must belong to the unprivileged account skep-api runs as", dir)
	}
	if err := e.drop(st.Uid, st.Gid); err != nil {
		return fmt.Errorf("cannot give up root for uid %d: %w", st.Uid, err)
	}
	return nil
}

// quietLogger is the logger of local commands: they report on stdout and
// stderr themselves, and only problems are logged.
func quietLogger(stderr io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}
