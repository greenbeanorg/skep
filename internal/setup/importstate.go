package setup

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The states of the import of a 0.1 host's users (ADR 0013 amendment 4,
// point 6), as "skep-api admin list --import-state" prints them.
// ImportUnmigrated is an api.db older than the installed skep-api (D18).
const (
	ImportPending    = "pending"
	ImportDone       = "done"
	ImportNotNeeded  = "not_needed"
	ImportSuperseded = "superseded"
	ImportUnmigrated = "unmigrated"
)

var importStates = []string{ImportPending, ImportDone, ImportNotNeeded, ImportSuperseded, ImportUnmigrated}

// ImportCommand is the command an operator runs to import the host's
// users, as root, as the docs print it (it gives up root for the
// owner of skep-api's state directory itself).
const ImportCommand = "skep-api admin import-users"

// Importer reads and finishes the import of the host's users through
// skep-api's local commands.
type Importer interface {
	// State is "skep-api admin list --import-state" (read-only).
	State(ctx context.Context) (string, error)
	// Import is "skep-api admin import-users", never with --yes.
	Import(ctx context.Context) (string, error)
}

// HostImporter runs the installed skep-api as the skep account, with the
// service's -data-dir and -config, with setup's fixed environment, reading
// at most 1 MiB of its output.
type HostImporter struct {
	Root   string
	Runner Runner
}

func (h HostImporter) run(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	cred, err := skepCredential(h.Root)
	if err != nil {
		return "", err
	}
	svc, err := apiServiceArgs(h.Root)
	if err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := h.Runner.Run(cctx, Cmd{Name: hostPath(h.Root, apiBin), Args: append(args, svc...), As: cred, Limit: maxCommandOutput})
	if err != nil {
		return CleanLines(strings.TrimSpace(out)), errors.New(CleanLines(err.Error()))
	}
	return CleanLines(strings.TrimSpace(out)), nil
}

// State implements Importer.
func (h HostImporter) State(ctx context.Context) (string, error) {
	out, err := h.run(ctx, 30*time.Second, "admin", "list", "--import-state")
	if err != nil {
		return "", fmt.Errorf("skep-api admin list --import-state: %w", err)
	}
	if !slices.Contains(importStates, out) {
		return "", fmt.Errorf("skep-api admin list --import-state printed %q", out)
	}
	return out, nil
}

// importTimeout bounds "skep-api admin import-users": it repeats its entry
// for up to 60 s while the outcome is unknown, then brings the copy up to
// date for up to 10 s.
const importTimeout = 2 * time.Minute

// Import implements Importer.
func (h HostImporter) Import(ctx context.Context) (string, error) {
	out, err := h.run(ctx, importTimeout, "admin", "import-users")
	if err != nil {
		return out, fmt.Errorf("skep-api admin import-users: %w", err)
	}
	return out, nil
}

// importWait is how long upgrade waits, after skep_api's start, for a
// state word (D13).
const importWait = 30 * time.Second

// waitForImportState polls the state every second for up to wait. Right
// after skep_api's start the state can read "unmigrated": the service has
// not run its migrations yet (seen on blazanov in the 8c-2 lab), so that
// word is waited out too, and is an error if it stays.
func waitForImportState(ctx context.Context, im Importer, wait time.Duration, sleep func(time.Duration)) (string, error) {
	var last error
	for waited := time.Duration(0); ; waited += time.Second {
		st, err := im.State(ctx)
		switch {
		case err == nil && st != ImportUnmigrated:
			return st, nil
		case err == nil:
			last = errors.New("api.db is still older than the installed skep-api (skep_api has not run its migrations)")
		default:
			last = err
		}
		if waited >= wait {
			return "", fmt.Errorf("no import state within %s: %v", wait, last)
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		sleep(time.Second)
	}
}
