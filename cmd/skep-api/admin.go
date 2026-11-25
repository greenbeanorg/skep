package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/apidb"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/cluster/userapi"
	"github.com/greenbeanorg/skep/internal/config"
	"github.com/greenbeanorg/skep/internal/termpass"
	"github.com/greenbeanorg/skep/internal/usercopy"
	"github.com/greenbeanorg/skep/pkg/models"
)

const adminUsage = `usage:
  skep-api admin create --username NAME [--display-name TEXT] [--password-stdin] [flags]
        create an administrator; the password is asked for twice on the terminal
  skep-api admin reset-password --username NAME [--password-stdin] [flags]
        set a new password for a user and end that user's sessions
  skep-api admin disable --username NAME [flags]
        disable a user and end that user's sessions
  skep-api admin delete --username NAME --yes [flags]
        delete a user and end that user's sessions
  skep-api admin exists --username NAME [flags]
        report whether a user exists, reading api.db without changing it;
        exit 0: it exists, 1: it does not, 2: error
  skep-api admin list [--import-state] [flags]
        list this node's users (name and role), reading api.db without changing it;
        with --import-state, print only the state of the import of its users
  skep-api admin import-users [--yes] [flags]
        make this host's 0.1 users the cluster's (once, on an upgraded host)
  skep-api admin hash-benchmark [flags]
        measure how long one password hash takes on this host

The password is never accepted as an argument or from the environment.
With --password-stdin it is read from standard input (one line).
The only enabled administrator can be neither disabled nor deleted.
Every change goes through skep-cluster and needs its quorum.`

// runAdmin dispatches the "admin" commands. They are the only way to
// create the first administrator and the documented recovery for a lost
// password; there are no default credentials and no network path
// (ADR 0006 decision 8).
func runAdmin(ctx context.Context, args []string, e env) error {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, adminUsage)
		return errors.New("missing admin command")
	}
	switch args[0] {
	case "create":
		return adminCreate(ctx, args[1:], e)
	case "reset-password":
		return adminResetPassword(ctx, args[1:], e)
	case "disable":
		return adminDisable(ctx, args[1:], e)
	case "delete":
		return adminDelete(ctx, args[1:], e)
	case "exists":
		return adminExists(ctx, args[1:], e)
	case "list":
		return adminList(ctx, args[1:], e)
	case "import-users":
		return adminImportUsers(ctx, args[1:], e)
	case "hash-benchmark":
		return adminHashBenchmark(ctx, args[1:], e)
	default:
		fmt.Fprintln(e.stderr, adminUsage)
		return fmt.Errorf("unknown admin command %q", args[0])
	}
}

func argon2Params(cfg config.API) auth.Params {
	return auth.Params{MemoryKiB: cfg.Auth.Argon2.MemoryKiB, Passes: cfg.Auth.Argon2.Passes, Lanes: auth.DefaultParams.Lanes}
}

// readNewPassword gets a new password: from standard input when asked to,
// otherwise twice from the terminal, without echo.
func readNewPassword(ctx context.Context, e env, fromStdin bool, username string) (string, error) {
	if fromStdin {
		line, err := bufio.NewReaderSize(io.LimitReader(e.stdin, termpass.MaxLength), termpass.MaxLength).ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("no password on standard input")
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	first, err := e.readPassword(ctx, fmt.Sprintf("New password for %s: ", username))
	if err != nil {
		if errors.Is(err, termpass.ErrNoTerminal) {
			return "", fmt.Errorf("%w; run this command on a terminal, or pass --password-stdin", err)
		}
		return "", err
	}
	// Check the rules before asking again.
	if err := auth.ValidatePassword(first); err != nil {
		return "", err
	}
	second, err := e.readPassword(ctx, "Repeat the password: ")
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errors.New("the two passwords are not the same; nothing was changed")
	}
	return first, nil
}

// adminSetup opens what the admin commands need. The caller closes db.
func adminSetup(ctx context.Context, cfg config.API, e env) (*auth.Users, func(), error) {
	log := quietLogger(e.stderr)
	db, err := openDatabase(ctx, cfg, log)
	if err != nil {
		return nil, nil, err
	}
	hasher, err := auth.NewHasher(argon2Params(cfg), 1, auth.DefaultHashWait)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	// The users are the cluster's (step 8c-1): a change goes through
	// skep-cluster, and the command brings this node's copy up to date
	// itself. The copy is read once first, so that the checks below see
	// the cluster's users; without quorum it stays as it is.
	store := apidb.NewUserStore(db)
	ucopy := usercopy.New(store, userapi.NewClient(cfg.ClusterSocket), log)
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_ = ucopy.SyncOnce(sctx, 0)
	cancel()
	users := auth.NewUsers(store, usercopy.NewCommandWriter(ucopy), hasher, apidb.NewAccessLog(db), log)
	return users, func() { db.Close() }, nil
}

func adminCreate(ctx context.Context, args []string, e env) error {
	flags := newLocalFlags("skep-api admin create", e.stderr)
	username := flags.set.String("username", "", "name of the administrator to create")
	displayName := flags.set.String("display-name", "", "full name, optional")
	fromStdin := flags.set.Bool("password-stdin", false, "read the password from standard input instead of the terminal")
	cfg, err := flags.prepare(args, e)
	if err != nil {
		return err
	}
	if err := auth.ValidateUsername(*username); err != nil {
		return err
	}
	users, done, err := adminSetup(ctx, cfg, e)
	if err != nil {
		return err
	}
	defer done()
	if _, err := users.ByName(ctx, *username); err == nil {
		return fmt.Errorf("user %q already exists; use 'skep-api admin reset-password' to set a new password", *username)
	} else if !errors.Is(err, models.ErrNotFound) {
		return err
	}
	password, err := readNewPassword(ctx, e, *fromStdin, *username)
	if err != nil {
		return err
	}
	user, err := users.Create(ctx, usercopy.LocalActor(e.uid), auth.NewUser{
		Username: *username, DisplayName: *displayName, Password: password, RoleID: auth.RoleAdmin,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "Administrator %q created (user ID %s).\n", user.Username, user.ID)
	return nil
}

func adminResetPassword(ctx context.Context, args []string, e env) error {
	flags := newLocalFlags("skep-api admin reset-password", e.stderr)
	username := flags.set.String("username", "", "name of the user whose password is replaced")
	fromStdin := flags.set.Bool("password-stdin", false, "read the password from standard input instead of the terminal")
	cfg, err := flags.prepare(args, e)
	if err != nil {
		return err
	}
	if err := auth.ValidateUsername(*username); err != nil {
		return err
	}
	users, done, err := adminSetup(ctx, cfg, e)
	if err != nil {
		return err
	}
	defer done()
	user, err := users.ByName(ctx, *username)
	if errors.Is(err, models.ErrNotFound) {
		return fmt.Errorf("there is no user %q", *username)
	}
	if err != nil {
		return err
	}
	password, err := readNewPassword(ctx, e, *fromStdin, *username)
	if err != nil {
		return err
	}
	if err := users.SetPassword(ctx, usercopy.LocalActor(e.uid), user.ID, password); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "The password of %q was replaced and all of that user's sessions were ended.\n", user.Username)
	if user.Disabled {
		fmt.Fprintf(e.stdout, "Note: %q is disabled and still cannot log in.\n", user.Username)
	}
	return nil
}

// lookupUser finds a user by name for a local command.
func lookupUser(ctx context.Context, users *auth.Users, username string) (auth.User, error) {
	user, err := users.ByName(ctx, username)
	if errors.Is(err, models.ErrNotFound) {
		return auth.User{}, fmt.Errorf("there is no user %q", username)
	}
	return user, err
}

// lastAdminError explains a refusal by the last-administrator rule
// (ADR 0006 decision 5) in terms of what to do about it.
func lastAdminError(username, verb string) error {
	return fmt.Errorf("%q is the only enabled administrator and cannot be %s; create another "+
		"administrator first (skep-api admin create --username NAME) or re-enable one through the API; nothing was changed", username, verb)
}

// adminDisable disables a user, which ends all of that user's sessions.
// The only enabled administrator is refused, as through the API.
func adminDisable(ctx context.Context, args []string, e env) error {
	flags := newLocalFlags("skep-api admin disable", e.stderr)
	username := flags.set.String("username", "", "name of the user to disable")
	cfg, err := flags.prepare(args, e)
	if err != nil {
		return err
	}
	if err := auth.ValidateUsername(*username); err != nil {
		return err
	}
	users, done, err := adminSetup(ctx, cfg, e)
	if err != nil {
		return err
	}
	defer done()
	user, err := lookupUser(ctx, users, *username)
	if err != nil {
		return err
	}
	if user.Disabled {
		fmt.Fprintf(e.stdout, "%q is already disabled; nothing was changed.\n", user.Username)
		return nil
	}
	disabled := true
	if _, err := users.Update(ctx, usercopy.LocalActor(e.uid), user.ID, auth.UserUpdate{Disabled: &disabled}); err != nil {
		if errors.Is(err, auth.ErrLastAdmin) {
			return lastAdminError(user.Username, "disabled")
		}
		return err
	}
	fmt.Fprintf(e.stdout, "%q was disabled and all of that user's sessions were ended.\n", user.Username)
	return nil
}

// adminDelete deletes a user and its sessions. It asks for --yes because it
// cannot be undone; the only enabled administrator is refused.
func adminDelete(ctx context.Context, args []string, e env) error {
	flags := newLocalFlags("skep-api admin delete", e.stderr)
	username := flags.set.String("username", "", "name of the user to delete")
	yes := flags.set.Bool("yes", false, "confirm the deletion, which cannot be undone")
	cfg, err := flags.prepare(args, e)
	if err != nil {
		return err
	}
	if err := auth.ValidateUsername(*username); err != nil {
		return err
	}
	users, done, err := adminSetup(ctx, cfg, e)
	if err != nil {
		return err
	}
	defer done()
	user, err := lookupUser(ctx, users, *username)
	if err != nil {
		return err
	}
	if !*yes {
		return fmt.Errorf("deleting %q (role %s) cannot be undone; run the command again with --yes to confirm",
			user.Username, user.Role.Name)
	}
	if err := users.Delete(ctx, usercopy.LocalActor(e.uid), user.ID); err != nil {
		if errors.Is(err, auth.ErrLastAdmin) {
			return lastAdminError(user.Username, "deleted")
		}
		return err
	}
	fmt.Fprintf(e.stdout, "%q was deleted and all of that user's sessions were ended.\n", user.Username)
	return nil
}

// adminHashBenchmark measures the configured Argon2id parameters on this
// host. ADR 0006 asks for 100 to 500 ms per hash; outside that, the pass
// count is what to change.
func adminHashBenchmark(ctx context.Context, args []string, e env) error {
	flags := newLocalFlags("skep-api admin hash-benchmark", e.stderr)
	rounds := flags.set.Int("rounds", 5, "number of hashes to time")
	cfg, err := flags.prepare(args, e)
	if err != nil {
		return err
	}
	if *rounds < 1 || *rounds > 100 {
		return errors.New("--rounds must be between 1 and 100")
	}
	params := argon2Params(cfg)
	// NewHasher computes the dummy hash: one hash before the timed ones.
	hasher, err := auth.NewHasher(params, 1, auth.DefaultHashWait)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "argon2id, %d MiB, %d passes, %d lane\n", params.MemoryKiB/1024, params.Passes, params.Lanes)
	var total, slowest time.Duration
	fastest := time.Hour
	for i := 0; i < *rounds; i++ {
		start := time.Now()
		if _, err := hasher.Hash(ctx, "a password used only for timing"); err != nil {
			return err
		}
		d := time.Since(start)
		total += d
		fastest, slowest = min(fastest, d), max(slowest, d)
		fmt.Fprintf(e.stdout, "  hash %d: %d ms\n", i+1, d.Milliseconds())
	}
	mean := total / time.Duration(*rounds)
	fmt.Fprintf(e.stdout, "mean %d ms, fastest %d ms, slowest %d ms\n", mean.Milliseconds(), fastest.Milliseconds(), slowest.Milliseconds())
	switch {
	case mean < 100*time.Millisecond:
		fmt.Fprintln(e.stdout, "Faster than the 100 to 500 ms target: consider raising auth.argon2.passes.")
	case mean > 500*time.Millisecond:
		fmt.Fprintln(e.stdout, "Slower than the 100 to 500 ms target: consider lowering auth.argon2.passes (not below 2).")
	default:
		fmt.Fprintln(e.stdout, "Within the 100 to 500 ms target.")
	}
	return nil
}
