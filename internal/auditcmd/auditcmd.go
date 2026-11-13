// Package auditcmd implements the "audit verify" and "audit export" host
// commands of skep-agent and skep-api (docs/decisions/0007-retention.md,
// decisions 3, 5, and 6). Each binary runs them on its own database and
// archive; the commands work whether or not the service is running, and
// they change nothing.
package auditcmd

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"

	"github.com/greenbeanorg/skep/internal/auditarchive"
	"github.com/greenbeanorg/skep/internal/database"
)

// Usage is the usage text of the commands, for program prog.
func Usage(prog string) string {
	return `usage:
  ` + prog + ` audit verify [--allow-missing] [flags]
        check the index of sealed audit segments and every segment file,
        and report the first break
  ` + prog + ` audit export [--month YYYY-MM] [flags]
        print the archived audit entries, one JSON object per line, after
        verifying the segments they come from

Segments are sealed monthly and never deleted by Skep. A segment that was
moved to other storage is reported as missing; verify accepts that only
with --allow-missing.`
}

// retentionSchema is the first schema version, of both databases, that has
// the segment index.
const retentionSchema = 3

// ErrNoIndex is returned by Open when the database predates the segment
// index: the service has not run since the upgrade, so nothing was sealed.
var ErrNoIndex = errors.New("this database has no audit segment index yet: " +
	"the service has not been started since retention was introduced, so nothing is archived")

// Open opens an existing database for the audit commands. Unlike the
// services it neither creates the file nor migrates the schema.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("there is no database at %s", path)
		}
		return nil, err
	}
	db, err := database.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	v, err := database.SchemaVersion(ctx, db)
	if err != nil {
		db.Close()
		return nil, err
	}
	if v < retentionSchema {
		db.Close()
		return nil, ErrNoIndex
	}
	return db, nil
}

// Verify checks the archive and prints one line per segment. It returns an
// error naming the first break; a missing file is a break unless
// allowMissing is set, and is printed either way.
func Verify(ctx context.Context, a *auditarchive.Archiver, allowMissing bool, out io.Writer) error {
	rep, err := a.Verify(ctx)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, c := range rep.Checks {
		counts[c.Status]++
		s := c.Segment
		line := fmt.Sprintf("%-8s %s  entries %d-%d (%d)  %s", c.Status, s.Month, s.FirstID, s.LastID, s.Entries, s.FileName)
		if c.Detail != "" {
			line += ": " + c.Detail
		}
		fmt.Fprintln(out, line)
	}
	for _, name := range rep.Unindexed {
		fmt.Fprintf(out, "%-8s %s: not a sealed segment (left by an interrupted run, or put here by hand); never deleted by Skep\n", "stray", name)
	}
	fmt.Fprintf(out, "%d sealed segments in %s: %d ok, %d missing, %d invalid\n", len(rep.Checks), a.Dir(),
		counts[auditarchive.StatusOK], counts[auditarchive.StatusMissing], counts[auditarchive.StatusInvalid])
	if len(rep.Checks) > 0 {
		last := rep.Checks[len(rep.Checks)-1].Segment
		fmt.Fprintf(out, "head of the chain: segment %d (%s) sha256 %s\n", last.Seq, last.Month, last.Hash)
	}
	return rep.Err(allowMissing)
}

var monthRE = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])$`)

// Export prints the archived entries, oldest first, one JSON object per
// line. month limits the output to one segment. Entries that are still in
// the live table are archived too once their month is sealed, and are
// printed like the others.
func Export(ctx context.Context, a *auditarchive.Archiver, month string, out io.Writer) error {
	if month != "" && !monthRE.MatchString(month) {
		return fmt.Errorf("--month %q is not a month in the form YYYY-MM", month)
	}
	w := bufio.NewWriterSize(out, 256<<10)
	segments, err := a.Export(ctx, month, func(line []byte) error {
		_, err := w.Write(line)
		return err
	})
	if err := errors.Join(err, w.Flush()); err != nil {
		return err
	}
	if segments == 0 && month != "" {
		return fmt.Errorf("there is no sealed segment for %s", month)
	}
	return nil
}
