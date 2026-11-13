// Command skep-api is the Skep API server. It is the only Skep process
// that listens on the network, and it runs as the unprivileged account
// "skep": it cannot change the host. Everything about the node is read
// from, and every operation is sent to, skep-agent over the agent's Unix
// socket (docs/decisions/0004-privilege-boundary.md,
// docs/decisions/0006-api-split-authentication-tls.md).
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/greenbeanorg/skep/internal/agentclient"
	"github.com/greenbeanorg/skep/internal/api"
	"github.com/greenbeanorg/skep/internal/apidb"
	"github.com/greenbeanorg/skep/internal/apitls"
	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/config"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/logging"
	"github.com/greenbeanorg/skep/internal/privdrop"
	"github.com/greenbeanorg/skep/internal/termpass"
	"github.com/greenbeanorg/skep/migrations"
	"github.com/greenbeanorg/skep/pkg/models"
	"github.com/greenbeanorg/skep/pkg/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := run(ctx, os.Args[1:], env{
		stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr,
		uid: os.Getuid(), euid: os.Geteuid(),
		drop:         privdrop.To,
		readPassword: termpass.Read,
	})
	switch {
	case err == nil:
	case errors.Is(err, flag.ErrHelp):
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "skep-api:", err)
		os.Exit(1)
	}
}

// env is what the program takes from its surroundings. Tests replace it.
type env struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	// uid and euid are the real and effective user IDs of the process.
	// The real one names the invoking user in the audit log.
	uid, euid int
	// drop gives up root for the given user and group.
	drop func(uid, gid uint32) error
	// readPassword asks for a password on the terminal, without echo.
	readPassword func(ctx context.Context, prompt string) (string, error)
}

// errRoot is returned when the server is started as root.
var errRoot = errors.New("refusing to run as root: skep-api is the network-facing process and " +
	"must run as the unprivileged skep account (use the skep_api rc.d service)")

// run is the whole program; main only maps its error to an exit code.
// Without a command it runs the server; "admin", "tls", and "audit" are
// local commands for the host's administrator.
func run(ctx context.Context, args []string, e env) error {
	if len(args) > 0 {
		switch args[0] {
		case "admin":
			return runAdmin(ctx, args[1:], e)
		case "tls":
			return runTLS(ctx, args[1:], e)
		case "audit":
			return runAudit(ctx, args[1:], e)
		}
	}
	return runServer(ctx, args, e)
}

func runServer(ctx context.Context, args []string, e env) error {
	flags := flag.NewFlagSet("skep-api", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	var (
		configPath  = flags.String("config", "", "path to the JSON configuration file (default "+config.DefaultAPIConfigPath+" if it exists)")
		dev         = flags.Bool("dev", false, "development mode: local data directory, text logs, debug level")
		listen      = flags.String("listen", "", "listen address (default "+config.DefaultAPIListenAddress+")")
		agentSocket = flags.String("agent-socket", "", "Unix socket of skep-agent (default "+config.DefaultSocketPath+")")
		dataDir     = flags.String("data-dir", "", "state directory")
		logLevel    = flags.String("log-level", "", "debug, info, warn, or error")
		logFormat   = flags.String("log-format", "", "json or text")
		showVersion = flags.Bool("version", false, "print the version and exit")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *showVersion {
		fmt.Fprintln(e.stdout, "skep-api", version.Get())
		return nil
	}
	if e.euid == 0 {
		return errRoot
	}

	overrides := config.APIOverrides{
		ListenAddress: *listen,
		AgentSocket:   *agentSocket,
		DataDir:       *dataDir,
		LogLevel:      *logLevel,
		LogFormat:     *logFormat,
	}
	// Only an explicit -dev overrides the file and environment.
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "dev" {
			overrides.DevMode = dev
		}
	})
	cfg, err := config.LoadAPI(config.APILoadOptions{Path: *configPath, Overrides: overrides})
	if err != nil {
		return err
	}

	log, err := logging.New(e.stderr, cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		return err
	}
	slog.SetDefault(log)
	log.Info("starting skep-api",
		"version", version.Version, "commit", version.Commit,
		"dev_mode", cfg.DevMode, "uid", e.euid, "agent_socket", cfg.AgentSocket)

	db, err := openDatabase(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer db.Close()
	access := apidb.NewAccessLog(db)
	logLocalActions(ctx, access, log)

	certs, err := apitls.New(ctx, apitls.Options{
		Dir:      cfg.TLSDir(),
		CertFile: cfg.TLS.CertFile,
		KeyFile:  cfg.TLS.KeyFile,
		Logger:   log,
		Audit: func(ctx context.Context, ev apitls.Event) {
			writeAudit(ctx, access, log, audit.AccessEntry{
				ActorName: audit.System, Action: ev.Action, ResourceType: "tls",
				Outcome: audit.Success, Details: ev.Details,
			})
		},
	})
	if err != nil {
		return err
	}

	// Identity: users, sessions, and the login limits.
	hasher, err := auth.NewHasher(argon2Params(cfg), auth.DefaultHashConcurrency, auth.DefaultHashWait)
	if err != nil {
		return err
	}
	userStore := apidb.NewUserStore(db)
	users := auth.NewUsers(userStore, hasher, access, log)
	throttle := auth.DefaultThrottleConfig()
	throttle.PerAddress = cfg.Auth.Login.PerAddressPerMinute
	throttle.FailuresBeforeDelay = cfg.Auth.Login.FailuresBeforeDelay
	throttle.MaxDelay = cfg.Auth.Login.MaxDelay.Std()
	throttle.PerUsername = cfg.Auth.Login.PerUsernameFailures
	authn, err := auth.NewAuthenticator(auth.AuthenticatorOptions{
		Users: userStore, Sessions: apidb.NewSessionStore(db), Hasher: hasher, Audit: access,
		Throttle: throttle,
		Session: auth.SessionConfig{
			IdleTimeout:     cfg.Auth.Session.IdleTimeout.Std(),
			AbsoluteTimeout: cfg.Auth.Session.AbsoluteTimeout.Std(),
		},
		Logger: log,
	})
	if err != nil {
		return err
	}
	proxies, err := cfg.TrustedProxyPrefixes()
	if err != nil {
		return err
	}
	if exists, err := users.AdminExists(ctx); err != nil {
		return err
	} else if !exists {
		log.Warn("no administrator exists yet: nobody can log in. Create one on this host with " +
			"'skep-api admin create --username NAME'")
	}
	go authn.Run(ctx, sessionSweepInterval)

	// The client does not connect here: the agent may start later, or
	// restart, and the API keeps answering in the meantime.
	agent, err := agentclient.New(agentclient.Options{SocketPath: cfg.AgentSocket})
	if err != nil {
		return err
	}
	defer agent.Close()

	// Retention of the access audit log: once now, then once a day.
	archive, err := newAccessArchive(db, cfg, agent, log, time.Now)
	if err != nil {
		return err
	}
	log.Info("retention configured", "audit_archive", cfg.Retention.ArchiveEnabled(),
		"audit_live_days", cfg.Retention.AuditLiveDaysOrZero(), "audit_archive_dir", cfg.AuditArchiveDir())
	// The loop is stopped, and waited for, before the database closes.
	archiveCtx, stopArchive := context.WithCancel(ctx)
	archiveDone := make(chan struct{})
	go func() {
		defer close(archiveDone)
		if archive != nil {
			archiveLoop(archiveCtx, archive, cfg.Retention.AuditLiveDaysOrZero(), access, log, archiveInterval, archiveRetry)
		}
	}()
	defer func() {
		stopArchive()
		<-archiveDone
	}()

	checks := map[string]models.HealthProbe{}
	if archive != nil {
		checks[models.CheckAccessAuditArchive] = archive.Health
	}
	handler := api.NewHandler(api.Options{
		Checks:   checks,
		Backend:  agent,
		Auth:     authn,
		Users:    users,
		Audit:    access,
		AuditLog: access,
		Logger:   log,
		TLS: func() models.TLSInfo {
			info := certs.Info()
			return models.TLSInfo{Source: string(info.Source), NotAfter: info.NotAfter.UTC(), PublicKeySHA256: info.PublicKeySHA256}
		},
		TrustedProxies: proxies,
	})
	err = serve(ctx, cfg, handler, certs, log)
	// Throttled logins of the current minute are counted, not yet
	// written: write them before the database closes.
	flushCtx, cancelFlush := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	authn.FlushThrottled(flushCtx, true)
	cancelFlush()
	if err != nil {
		return err
	}
	log.Info("stopped")
	return nil
}

// sessionSweepInterval is how often expired sessions are deleted.
const sessionSweepInterval = 5 * time.Minute

// openDatabase opens api.db and brings its schema up to date.
func openDatabase(ctx context.Context, cfg config.API, log *slog.Logger) (*sql.DB, error) {
	db, err := database.Open(ctx, cfg.Database.Path)
	if err != nil {
		return nil, err
	}
	applied, err := database.Migrate(ctx, db, migrations.API())
	if err != nil {
		db.Close()
		return nil, err
	}
	schema, err := database.SchemaVersion(ctx, db)
	if err != nil {
		db.Close()
		return nil, err
	}
	log.Info("database ready", "path", cfg.Database.Path, "schema_version", schema, "migrations_applied", len(applied))
	return db, nil
}

// logLocalActions logs what was done with local commands since skep-api
// last started, so that an administrator created or a key replaced while
// the server was down still appears in its log. It then records this
// start.
func logLocalActions(ctx context.Context, access *apidb.AccessLog, log *slog.Logger) {
	actions, err := access.LocalSinceLastStart(ctx)
	if err != nil {
		log.Error("could not read the local actions from the access audit log", "error", err)
	}
	for _, a := range actions {
		log.Info("local command since the last start",
			"at", a.At, "actor", a.Actor, "action", a.Action, "resource_id", a.ResourceID, "details", a.Details)
	}
	writeAudit(ctx, access, log, audit.AccessEntry{
		ActorName: audit.System, Action: apidb.ActionAPIStart, Outcome: audit.Success,
		Details: map[string]any{"version": version.Version},
	})
}

// writeAudit appends to the access audit log. A failure is logged with the
// entry, so the event is not lost, and does not stop the caller.
func writeAudit(ctx context.Context, w *apidb.AccessLog, log *slog.Logger, e audit.AccessEntry) {
	if err := w.Write(ctx, e); err != nil {
		log.Error("could not write to the access audit log", "error", err,
			"action", e.Action, "actor", e.ActorName, "outcome", string(e.Outcome))
	}
}

// certReloadInterval is how often the certificate files are checked for
// changes and the expiry is checked. SIGHUP does both at once.
const certReloadInterval = time.Minute

// serve runs the HTTPS server until ctx is cancelled, then shuts it down
// gracefully. There is no plaintext listener.
func serve(ctx context.Context, cfg config.API, handler http.Handler, certs *apitls.Manager, log *slog.Logger) error {
	ln, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddress, err)
	}
	srv := &http.Server{
		Handler:           handler,
		TLSConfig:         certs.TLSConfig(),
		ReadHeaderTimeout: cfg.ReadTimeout.Std(),
		ReadTimeout:       cfg.ReadTimeout.Std(),
		WriteTimeout:      cfg.WriteTimeout.Std(),
		IdleTimeout:       2 * cfg.WriteTimeout.Std(),
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}

	// SIGHUP reloads the certificate from disk and checks its expiry.
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	defer signal.Stop(sighup)
	hup := make(chan struct{}, 1)
	upkeepCtx, stopUpkeep := context.WithCancel(ctx)
	defer stopUpkeep()
	go func() {
		for {
			select {
			case <-upkeepCtx.Done():
				return
			case <-sighup:
				select {
				case hup <- struct{}{}:
				default:
				}
			}
		}
	}()
	go certs.Run(upkeepCtx, certReloadInterval, hup)

	errc := make(chan error, 1)
	// The certificate comes from TLSConfig.GetCertificate.
	go func() { errc <- srv.ServeTLS(ln, "", "") }()
	info := certs.Info()
	log.Info("API listening", "address", ln.Addr().String(), "base_path", api.Prefix,
		"tls", "1.3", "certificate", string(info.Source), "public_key", info.PublicKeySHA256,
		"authentication", "sessions")
	if host, _, err := net.SplitHostPort(cfg.ListenAddress); err == nil && !config.IsLoopbackHost(host) {
		log.Info("the API is reachable from the network, as configured", "address", ln.Addr().String())
	}

	select {
	case err := <-errc:
		return fmt.Errorf("https server: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout.Std())
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("https server shutdown: %w", err)
	}
	return nil
}
