// Command skep-api is the Skep API server. It is the only Skep process
// that listens on the network, and it runs as the unprivileged account
// "skep": it cannot change the host. Everything about the node is read
// from, and every operation is sent to, skep-agent over the agent's Unix
// socket (docs/decisions/0004-privilege-boundary.md,
// docs/decisions/0006-api-split-authentication-tls.md).
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
	"syscall"

	"github.com/greenbeanorg/skep/internal/agentclient"
	"github.com/greenbeanorg/skep/internal/api"
	"github.com/greenbeanorg/skep/internal/config"
	"github.com/greenbeanorg/skep/internal/logging"
	"github.com/greenbeanorg/skep/pkg/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := run(ctx, os.Args[1:], env{stdout: os.Stdout, stderr: os.Stderr, euid: os.Geteuid()})
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
	stdout, stderr io.Writer
	// euid is the effective user ID of the process.
	euid int
}

// errRoot is returned when the server is started as root.
var errRoot = errors.New("refusing to run as root: skep-api is the network-facing process and " +
	"must run as the unprivileged skep account (use the skep_api rc.d service)")

// run is the whole program; main only maps its error to an exit code.
func run(ctx context.Context, args []string, e env) error {
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

	// The client does not connect here: the agent may start later, or
	// restart, and the API keeps answering in the meantime.
	agent, err := agentclient.New(agentclient.Options{SocketPath: cfg.AgentSocket})
	if err != nil {
		return err
	}
	defer agent.Close()

	if err := serve(ctx, cfg, api.NewHandler(agent, log), log); err != nil {
		return err
	}
	log.Info("stopped")
	return nil
}

// serve runs the HTTP server until ctx is cancelled, then shuts it down
// gracefully.
func serve(ctx context.Context, cfg config.API, handler http.Handler, log *slog.Logger) error {
	ln, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddress, err)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadTimeout.Std(),
		ReadTimeout:       cfg.ReadTimeout.Std(),
		WriteTimeout:      cfg.WriteTimeout.Std(),
		IdleTimeout:       2 * cfg.WriteTimeout.Std(),
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout.Std())
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http server shutdown: %w", err)
	}
	return nil
}
