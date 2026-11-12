// Command skep-agent is the Skep host agent. It runs on every node,
// discovers the host read-only, keeps local state in SQLite, and, in this
// release, serves the REST API under /api/v1.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/greenbeanorg/skep/internal/agent"
	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/api"
	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/config"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/hardware"
	"github.com/greenbeanorg/skep/internal/internalapi"
	"github.com/greenbeanorg/skep/internal/logging"
	"github.com/greenbeanorg/skep/internal/sysaccount"
	"github.com/greenbeanorg/skep/internal/tasks"
	"github.com/greenbeanorg/skep/migrations"
	"github.com/greenbeanorg/skep/pkg/models"
	"github.com/greenbeanorg/skep/pkg/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	switch {
	case err == nil:
	case errors.Is(err, flag.ErrHelp):
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "skep-agent:", err)
		os.Exit(1)
	}
}

// run is the whole program; main only maps its error to an exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("skep-agent", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var (
		configPath  = flags.String("config", "", "path to the JSON configuration file (default "+config.DefaultConfigPath+" if it exists)")
		dev         = flags.Bool("dev", false, "development mode: local data directory, text logs, debug level")
		listen      = flags.String("listen", "", "API listen address (default "+config.DefaultListenAddress+")")
		socketPath  = flags.String("socket", "", "Unix socket of the internal API (default "+config.DefaultSocketPath+")")
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
		fmt.Fprintln(stdout, "skep-agent", version.Get())
		if testTasksBuilt {
			fmt.Fprintln(stdout, "TEST BUILD: contains test-only task types; not for installation")
		}
		return nil
	}

	overrides := config.Overrides{
		ListenAddress: *listen,
		SocketPath:    *socketPath,
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
	cfg, err := config.Load(config.LoadOptions{Path: *configPath, Overrides: overrides})
	if err != nil {
		return err
	}

	log, err := logging.New(stderr, cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		return err
	}
	slog.SetDefault(log)
	log.Info("starting skep-agent",
		"version", version.Version, "commit", version.Commit,
		"dev_mode", cfg.DevMode, "data_dir", cfg.DataDir)

	db, err := database.Open(ctx, cfg.Database.Path)
	if err != nil {
		return err
	}
	defer db.Close()

	applied, err := database.Migrate(ctx, db, migrations.Agent())
	if err != nil {
		return err
	}
	schema, err := database.SchemaVersion(ctx, db)
	if err != nil {
		return err
	}
	log.Info("database ready", "path", cfg.Database.Path, "schema_version", schema, "migrations_applied", len(applied))

	disc := hardware.New(hardware.Options{CommandTimeout: cfg.Discovery.CommandTimeout.Std()})
	hostname, _ := os.Hostname()
	nodeID, err := database.EnsureLocalNode(ctx, db, hostname)
	if err != nil {
		return err
	}
	if runtime.GOOS != "freebsd" {
		log.Warn("this is not a FreeBSD host: running as a development build only, never as a production host")
	}

	engine, err := tasks.New(tasks.Options{
		Store:     database.NewTaskStore(db),
		Audit:     database.NewAuditLog(db),
		NodeID:    nodeID,
		Workers:   cfg.Tasks.Workers,
		QueueSize: cfg.Tasks.QueueSize,
		Logger:    log,
	})
	if err != nil {
		return err
	}
	svc, err := agent.NewService(agent.Options{
		NodeID:     nodeID,
		DevMode:    cfg.DevMode,
		Discoverer: disc,
		Database:   db,
		Nodes:      database.NewNodeStore(db),
		Tasks:      engine,
		CacheTTL:   cfg.Discovery.CacheTTL.Std(),
		Logger:     log,
	})
	if err != nil {
		return err
	}
	if err := engine.Register(svc.RefreshDefinition()); err != nil {
		return err
	}
	// Nothing in a shipped build; see testtasks.go.
	if err := registerTestTasks(engine); err != nil {
		return err
	}

	// Start marks tasks left unfinished by a previous run as interrupted.
	// It must finish before anything can be submitted or served.
	if err := engine.Start(ctx); err != nil {
		return err
	}

	// Refresh the node inventory once at startup, as a task, so it leaves
	// the same history and audit trail as any other operation. A refusal
	// here is not fatal: the agent must stay up.
	if _, _, err := engine.Submit(ctx, tasks.Request{
		Type:         agent.TaskDiscoveryRefresh,
		ResourceType: agent.NodeResource,
		ResourceID:   nodeID,
		Actor:        models.TaskActor{ID: audit.System, Name: audit.System},
		PeerUID:      uint32(os.Geteuid()),
	}); err != nil {
		log.Warn("could not submit the initial discovery task", "error", err)
	}
	submitTestTasks(ctx, engine, log)

	// The internal API on the Unix socket is how skep-api reaches the
	// agent. The public API on loopback TCP is still served next to it
	// until skep-api takes it over.
	//
	// Until then a socket that cannot be created is not fatal: under the
	// Milestone 3 rc.d service the agent still runs as skep and cannot
	// write to the root-owned run directory.
	socket, err := listenSocket(cfg, log)
	if err != nil {
		log.Warn("the internal API is not available", "error", err)
		socket = nil
	}
	internal := internalapi.NewHandler(svc, internalapi.Options{
		RequestTimeout: cfg.Socket.RequestTimeout.Std(),
		Logger:         log,
	})
	serveErr := serve(ctx, cfg, api.NewHandler(svc, log), socket, internal, log)

	// Stop the engine after the listener: running tasks are told to stop
	// and are recorded as interrupted.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.API.ShutdownTimeout.Std())
	defer cancel()
	if err := engine.Shutdown(shutdownCtx); err != nil {
		log.Warn("task engine did not stop cleanly; unfinished tasks will be marked interrupted at the next start", "error", err)
	}
	if serveErr == nil {
		log.Info("stopped")
	}
	return serveErr
}

// listenSocket creates the agent socket with the configured group and
// peer allow-list.
func listenSocket(cfg config.Config, log *slog.Logger) (*internalapi.Listener, error) {
	uids, err := sysaccount.UIDs(cfg.Socket.AllowedUsers)
	if err != nil {
		return nil, fmt.Errorf("socket.allowed_users: %w", err)
	}
	gid, mode := -1, fs.FileMode(0o600)
	if cfg.Socket.Group != "" {
		g, err := sysaccount.GID(cfg.Socket.Group)
		if err != nil {
			return nil, fmt.Errorf("socket.group: %w", err)
		}
		gid, mode = int(g), 0o660
	}
	if cfg.DevMode {
		// In production the rc.d script creates the run directory as
		// root:skep 0750. In dev mode it lives in the data directory.
		if err := os.MkdirAll(filepath.Dir(cfg.Socket.Path), 0o700); err != nil {
			return nil, fmt.Errorf("create the socket directory: %w", err)
		}
	}
	ln, err := internalapi.Listen(internalapi.ListenOptions{
		Path:           cfg.Socket.Path,
		Mode:           mode,
		GID:            gid,
		AllowedUIDs:    uids,
		MaxConnections: cfg.Socket.MaxConnections,
		Logger:         log,
	})
	if err != nil {
		return nil, err
	}
	log.Info("internal API listening", "socket", cfg.Socket.Path, "base_path", agentproto.Prefix,
		"mode", fmt.Sprintf("%04o", mode), "group", cfg.Socket.Group, "allowed_uids", uids)
	return ln, nil
}

// serve runs the public API on loopback TCP and the internal API on the
// agent socket until ctx is cancelled or one of them fails, then shuts
// both down gracefully.
func serve(ctx context.Context, cfg config.Config, public http.Handler, socket *internalapi.Listener, internal http.Handler, log *slog.Logger) error {
	if socket != nil {
		defer socket.Close()
	}
	ln, err := net.Listen("tcp", cfg.API.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.API.ListenAddress, err)
	}
	if host, _, err := net.SplitHostPort(cfg.API.ListenAddress); err == nil && !config.IsLoopbackHost(host) {
		log.Warn("the API is listening on a non-loopback address without authentication or TLS",
			"address", ln.Addr().String())
	}

	srv := &http.Server{
		Handler:           public,
		ReadHeaderTimeout: cfg.API.ReadTimeout.Std(),
		ReadTimeout:       cfg.API.ReadTimeout.Std(),
		WriteTimeout:      cfg.API.WriteTimeout.Std(),
		IdleTimeout:       2 * cfg.API.WriteTimeout.Std(),
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	internalSrv := internalapi.NewServer(internal, cfg.Socket.RequestTimeout.Std(), log)

	errc := make(chan error, 2)
	go func() { errc <- srv.Serve(ln) }()
	if socket != nil {
		go func() { errc <- internalSrv.Serve(socket) }()
	}
	log.Info("API listening", "address", ln.Addr().String(), "base_path", api.Prefix, "authentication", "none")

	var serveErr error
	select {
	case err := <-errc:
		serveErr = fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.API.ShutdownTimeout.Std())
	defer cancel()
	err = errors.Join(srv.Shutdown(shutdownCtx), internalSrv.Shutdown(shutdownCtx))
	if serveErr != nil {
		return serveErr
	}
	if err != nil {
		return fmt.Errorf("http server shutdown: %w", err)
	}
	return nil
}
