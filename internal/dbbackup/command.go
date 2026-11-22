package dbbackup

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
)

// Paths are what a program's configuration says about its database.
type Paths struct {
	DBPath     string
	ArchiveDir string
	DataDir    string
}

// Resolve loads a program's configuration from the -config and -data-dir
// flags and returns its paths.
type Resolve func(configPath, dataDir string) (Paths, error)

// Usage is the help of the db command.
func Usage(program string) string {
	return fmt.Sprintf(`usage: %s db backup [-dir DIR] [-keep N] [-config FILE] [-data-dir DIR]

Copies the database consistently (SQLite VACUUM INTO, read-only on the
source; the service may keep running) into DIR, by default
<data dir>/backup/<version>+<commit>-<UTC timestamp>, with its SHA-256 and a listing
of the audit archive (not a copy). Keeps the newest N backups (default %d)
and removes older ones made by this command.`, program, DefaultKeep)
}

var versionLabelRE = regexp.MustCompile(`[^A-Za-z0-9._+-]`)

// Label is a backup directory's label for a build: <version>+<commit>,
// the form skep-setup upgrade uses for the build it backs up, with any
// character unsafe in a file name replaced by "_".
func Label(version, commit string) string {
	return versionLabelRE.ReplaceAllString(version+"+"+commit, "_")
}

// RunCommand is "<program> db ..." for skep-agent (name "agent") and
// skep-api (name "api"). label names the running build (Label).
func RunCommand(ctx context.Context, program, name, label string, args []string, stdout, stderr io.Writer, resolve Resolve) error {
	if len(args) == 0 || args[0] != "backup" {
		fmt.Fprintln(stderr, Usage(program))
		if len(args) == 0 {
			return errors.New("missing db command")
		}
		return fmt.Errorf("unknown db command %q", args[0])
	}
	flags := flag.NewFlagSet(program+" db backup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "path to the JSON configuration file")
	dataDir := flags.String("data-dir", "", "state directory")
	dir := flags.String("dir", "", "backup directory, <data dir>/backup/<version>+<commit>-<UTC timestamp> (default: a new one)")
	keep := flags.Int("keep", DefaultKeep, "how many backups to keep")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	p, err := resolve(*configPath, *dataDir)
	if err != nil {
		return err
	}
	r, err := Backup(ctx, Options{
		Name: name, DBPath: p.DBPath, ArchiveDir: p.ArchiveDir,
		Parent: filepath.Join(p.DataDir, "backup"), Dir: *dir,
		Label: versionLabelRE.ReplaceAllString(label, "_"), Keep: *keep,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s db backup: %s sha256 %s\n", program, r.File, r.SHA256)
	if r.Archive != "" {
		fmt.Fprintf(stdout, "%s db backup: audit archive listed in %s\n", program, r.Archive)
	}
	for _, d := range r.Pruned {
		fmt.Fprintf(stdout, "%s db backup: removed the old backup %s\n", program, d)
	}
	return nil
}
