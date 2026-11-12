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
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/greenbeanorg/skep/internal/agent"
	"github.com/greenbeanorg/skep/internal/api"
	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/config"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/hardware"
	"github.com/greenbeanorg/skep/internal/logging"
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
	fs := flag.NewFlagSet("skep-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		configPath  = fs.String("config", "", "path to the JSON configuration file (default "+config.DefaultConfigPath+" if it exists)")
		dev         = fs.Bool("dev", false, "development mode: local data directory, text logs, debug level")
		listen      = fs.String("listen", "", "API listen address (default "+config.DefaultListenAddress+")")
		dataDir     = fs.String("data-dir", "", "state directory")
		logLevel    = fs.String("log-level", "", "debug, info, warn, or error")
		logFormat   = fs.String("log-format", "", "json or text")
		showVersion = fs.Bool("version", false, "print the version and exit")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *showVersion {
		fmt.Fprintln(stdout, "skep-agent", version.Get())
		return nil
	}

	overrides := config.Overrides{
		ListenAddress: *listen,
		DataDir:       *dataDir,
		LogLevel:      *logLevel,
		LogFormat:     *logFormat,
	}
	// Only an explicit -dev overrides the file and environment.
	fs.Visit(func(f *flag.Flag) {
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

	serveErr := serve(ctx, cfg, api.NewHandler(svc, log), log)

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

// serve runs the HTTP server until ctx is cancelled, then shuts it down
// gracefully.
func serve(ctx context.Context, cfg config.Config, handler http.Handler, log *slog.Logger) error {
	ln, err := net.Listen("tcp", cfg.API.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.API.ListenAddress, err)
	}
	if host, _, err := net.SplitHostPort(cfg.API.ListenAddress); err == nil && !config.IsLoopbackHost(host) {
		log.Warn("the API is listening on a non-loopback address without authentication or TLS",
			"address", ln.Addr().String())
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: cfg.API.ReadTimeout.Std(),
		ReadTimeout:       cfg.API.ReadTimeout.Std(),
		WriteTimeout:      cfg.API.WriteTimeout.Std(),
		IdleTimeout:       2 * cfg.API.WriteTimeout.Std(),
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("API listening", "address", ln.Addr().String(), "base_path", api.Prefix, "authentication", "none")

	select {
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.API.ShutdownTimeout.Std())
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http server shutdown: %w", err)
	}
	return nil
}
