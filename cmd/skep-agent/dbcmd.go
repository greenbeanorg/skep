package main

import (
	"context"
	"io"

	"github.com/greenbeanorg/skep/internal/config"
	"github.com/greenbeanorg/skep/internal/dbbackup"
	"github.com/greenbeanorg/skep/pkg/version"
)

// runDB is "skep-agent db backup" (ADR 0012 Decision 5).
func runDB(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	v := version.Get()
	return dbbackup.RunCommand(ctx, "skep-agent", "agent", v.Version+"-"+v.Commit, args, stdout, stderr,
		func(configPath, dataDir string) (dbbackup.Paths, error) {
			cfg, err := config.Load(config.LoadOptions{Path: configPath, Overrides: config.Overrides{DataDir: dataDir}})
			if err != nil {
				return dbbackup.Paths{}, err
			}
			return dbbackup.Paths{DBPath: cfg.Database.Path, ArchiveDir: cfg.AuditArchiveDir(), DataDir: cfg.DataDir}, nil
		})
}
