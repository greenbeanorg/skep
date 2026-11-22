package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"

	"github.com/greenbeanorg/skep/internal/auth"
)

// Exit codes of "skep-api admin exists". skep-setup's planner tells them
// apart, so they are part of that command's interface.
const (
	existsYes   = 0 // the user exists
	existsNo    = 1 // there is no such user
	existsError = 2 // the question could not be answered
)

// exitCodeError makes main exit with code. An empty message prints
// nothing more (the command has already said what it found).
type exitCodeError struct {
	code int
	msg  string
}

func (e *exitCodeError) Error() string { return e.msg }

// adminExists answers whether a user exists, for skep-setup (ADR 0012,
// finding 1 of PR #52). It opens api.db read-only and only if it exists:
// it never creates the database or its directory and never migrates the
// schema, so asking can not change the host. Exit 0: the user exists; 1:
// it does not; 2: anything else.
func adminExists(ctx context.Context, args []string, e env) error {
	flags := newLocalFlags("skep-api admin exists", e.stderr)
	username := flags.set.String("username", "", "name of the user to look for")
	fail := func(err error) error { return &exitCodeError{code: existsError, msg: err.Error()} }
	cfg, err := flags.prepare(args, e)
	if err != nil {
		return fail(err)
	}
	if err := auth.ValidateUsername(*username); err != nil {
		return fail(err)
	}
	path := cfg.Database.Path
	st, err := os.Lstat(path)
	if err != nil {
		return fail(fmt.Errorf("cannot read %s: %w", path, err))
	}
	if !st.Mode().IsRegular() {
		return fail(fmt.Errorf("%s is not a regular file", path))
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?"+q.Encode())
	if err != nil {
		return fail(fmt.Errorf("open %s: %w", path, err))
	}
	defer db.Close()
	var (
		role     string
		disabled int
	)
	err = db.QueryRowContext(ctx, `SELECT r.name, u.disabled FROM users u JOIN roles r ON r.id = u.role_id
		WHERE u.username = ?`, *username).Scan(&role, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		fmt.Fprintf(e.stdout, "there is no user %q\n", *username)
		return &exitCodeError{code: existsNo}
	}
	if err != nil {
		return fail(fmt.Errorf("read %s: %w", path, err))
	}
	state := ""
	if disabled != 0 {
		state = ", disabled"
	}
	fmt.Fprintf(e.stdout, "user %q exists (role %s%s)\n", *username, role, state)
	return nil
}
