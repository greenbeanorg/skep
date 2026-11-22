package main

import (
	"context"

	"github.com/greenbeanorg/skep/internal/config"
	"github.com/greenbeanorg/skep/internal/dbbackup"
	"github.com/greenbeanorg/skep/pkg/version"
)

// runDB is "skep-api db backup" (ADR 0012 Decision 5). Unlike the server
// it may run as root: it only reads api.db and writes the copy 0600.
func runDB(ctx context.Context, args []string, e env) error {
	v := version.Get()
	return dbbackup.RunCommand(ctx, "skep-api", "api", dbbackup.Label(v.Version, v.Commit), args, e.stdout, e.stderr,
		func(configPath, dataDir string) (dbbackup.Paths, error) {
			cfg, err := config.LoadAPI(config.APILoadOptions{Path: configPath, Overrides: config.APIOverrides{DataDir: dataDir}})
			if err != nil {
				return dbbackup.Paths{}, err
			}
			return dbbackup.Paths{DBPath: cfg.Database.Path, ArchiveDir: cfg.AuditArchiveDir(), DataDir: cfg.DataDir}, nil
		})
}
