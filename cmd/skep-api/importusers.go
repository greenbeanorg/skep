package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/apidb"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/userapi"
	"github.com/greenbeanorg/skep/internal/ids"
	"github.com/greenbeanorg/skep/internal/usercopy"
	"github.com/greenbeanorg/skep/pkg/models"
)

// importRepeat is how long the import repeats its entry while the outcome
// is unknown.
const importRepeat = 60 * time.Second

// sizeOrigin stands for the origin node's ID when the entry's size is
// measured: a node ID has this length.
const sizeOrigin = "00000000-0000-0000-0000-000000000000"

// adminImportUsers makes this host's 0.1 users the cluster's (step 8c-1,
// D14). It records the operation ID in api.db before it sends the one
// users.import entry, and repeats it with the same ID while the outcome is
// unknown, so a rerun after a crash finishes. A cluster that has users
// already refuses it: their users replace this host's, once confirmed.
func adminImportUsers(ctx context.Context, args []string, e env) error {
	flags := newLocalFlags("skep-api admin import-users", e.stderr)
	yes := flags.set.Bool("yes", false, "confirm that the cluster's users replace this host's, if the cluster has users already")
	cfg, err := flags.prepare(args, e)
	if err != nil {
		return err
	}
	log := quietLogger(e.stderr)
	db, err := openDatabase(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer db.Close()
	store := apidb.NewUserStore(db)
	ucopy := usercopy.New(store, userapi.NewClient(cfg.ClusterSocket), log)
	writer := usercopy.NewCommandWriter(ucopy)
	cs, err := store.CopyState(ctx)
	if err != nil {
		return err
	}
	switch cs.Import {
	case apidb.ImportNotNeeded:
		fmt.Fprintln(e.stdout, "This host had no users to import; its users are the cluster's.")
		return nil
	case apidb.ImportDone:
		fmt.Fprintln(e.stdout, "This host's users were imported already.")
		return nil
	case apidb.ImportSuperseded:
		fmt.Fprintln(e.stdout, "This host's users were replaced by the cluster's already.")
		return nil
	}
	local, err := store.LocalImportUsers(ctx)
	if err != nil {
		return err
	}
	// The size is checked before anything is recorded or sent (D7).
	probe, err := clusterstate.NewEntry(clusterstate.TypeUsersImport, cs.ImportOpID+"x", usercopy.ActorString(usercopy.LocalActor(e.uid)),
		sizeOrigin, clusterstate.UsersImport{Users: local, At: time.Now().Unix()})
	if err != nil {
		return err
	}
	if raw, err := probe.Encode(); err != nil || len(raw) > clusterstate.MaxImportBytes {
		return fmt.Errorf("the import of %d users is larger than one entry may be (%d KiB); nothing was changed", len(local), clusterstate.MaxImportBytes>>10)
	}
	opID := cs.ImportOpID
	if opID == "" {
		fresh, err := ids.NewUUID()
		if err != nil {
			return err
		}
		if opID, err = store.RecordImportOp(ctx, fresh); err != nil {
			return err
		}
	}
	data, err := json.Marshal(clusterstate.UsersImport{Users: local})
	if err != nil {
		return err
	}
	req := userapi.UsersWriteRequest{Type: clusterstate.TypeUsersImport, OpID: opID, Actor: usercopy.ActorString(usercopy.LocalActor(e.uid)), Data: data}
	_, err = writer.Send(ctx, req, importRepeat)
	switch {
	case err == nil:
		if err := store.FinishImport(ctx, apidb.ImportDone); err != nil {
			return err
		}
		syncCopy(ctx, ucopy)
		fmt.Fprintf(e.stdout, "This host's %d users are now the cluster's (operation %s).\n", len(local), opID)
		return nil
	case errors.Is(err, usercopy.ErrUsersExist):
		names := make([]string, 0, len(local))
		for _, u := range local {
			names = append(names, u.Username)
		}
		fmt.Fprintf(e.stdout, "The cluster has users already. Its users replace this host's: %s.\n", strings.Join(names, ", "))
		fmt.Fprintln(e.stdout, "Every session on this host ends, and these accounts exist here only if the cluster has them.")
		if !*yes {
			return errors.New("not confirmed: run the command again with --yes to let the cluster's users replace this host's; nothing was changed")
		}
		if err := store.FinishImport(ctx, apidb.ImportSuperseded); err != nil {
			return err
		}
		syncCopy(ctx, ucopy)
		fmt.Fprintln(e.stdout, "This host now uses the cluster's users.")
		return nil
	}
	if errors.Is(err, &models.Error{Code: auth.CodeNoQuorum}) {
		return fmt.Errorf("%w; the operation ID is recorded: run the command again, it finishes the import", err)
	}
	// A refusal applied nothing: the recorded operation ID is dropped.
	if cerr := store.ClearImportOp(ctx); cerr != nil {
		return cerr
	}
	return fmt.Errorf("the import was refused: %w; nothing was changed", err)
}

// syncCopy brings the copy up to date, for up to 10 s.
func syncCopy(ctx context.Context, c *usercopy.Copy) {
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = c.SyncOnce(sctx, 0)
}

// adminList lists this node's users for "skep-cluster join" (D21): names
// and roles, from api.db opened read-only. It changes nothing.
func adminList(ctx context.Context, args []string, e env) error {
	flags := newLocalFlags("skep-api admin list", e.stderr)
	cfg, err := flags.prepare(args, e)
	if err != nil {
		return err
	}
	path := cfg.Database.Path
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // no database: no users
	}
	if err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a readable regular file", path)
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?"+q.Encode())
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT username, role_id, disabled FROM users ORDER BY username`)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, role string
		var disabled int
		if err := rows.Scan(&name, &role, &disabled); err != nil {
			return err
		}
		line := name + " " + role
		if disabled != 0 {
			line += " disabled"
		}
		fmt.Fprintln(e.stdout, line)
	}
	return rows.Err()
}
