package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/service"
	"github.com/greenbeanorg/skep/internal/cluster/store"
	"github.com/greenbeanorg/skep/internal/logging"
	"github.com/greenbeanorg/skep/pkg/version"
)

// errRoot is returned when the service is started as root.
var errRoot = errors.New("refusing to run as root: the service holds the node key and listens on the " +
	"network; it must run as the unprivileged skep-cluster account (use the skep_cluster rc.d service)")

func runServe(ctx context.Context, args []string, e env) error {
	flags := flag.NewFlagSet("skep-cluster", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	var (
		configPath = flags.String("config", clusterconf.DefaultPath, "path to cluster.json")
		dataDir    = flags.String("data-dir", clusterconf.DefaultDataDir, "directory of the node key and the cluster store")
		socket     = flags.String("socket", clusterconf.DefaultSocket, "Unix socket of the local API")
		apiAccount = flags.String("api-account", clusterconf.APIAccount, "the account skep-api runs as; with root, the only caller of the socket")
		logLevel   = flags.String("log-level", "info", "debug, info, warn or error")
		logFormat  = flags.String("log-format", "json", "json or text")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if e.euid == 0 {
		return errRoot
	}
	log, err := logging.New(e.stderr, *logLevel, *logFormat)
	if err != nil {
		return err
	}
	cfg, err := clusterconf.Load(*configPath)
	if err != nil {
		return err
	}
	api, err := e.lookup(*apiAccount)
	if err != nil {
		return fmt.Errorf("the API account: %w", err)
	}
	if api.UID == 0 {
		return errors.New("the API account must not be root")
	}
	key, repaired, err := nodekey.LoadRepair(*dataDir, cfg.NodeID, time.Now())
	if err != nil {
		return fmt.Errorf("%w (run \"skep-cluster init\" as root if this host was never initialised)", err)
	}
	if repaired != nil {
		log.Warn("the node certificate could not be used; a new one was issued with the same key, the pin is unchanged",
			"reason", repaired.Error(), "pin", key.Pin(), "not_after", key.NotAfter())
	}
	node, err := store.Open(store.Options{Dir: *dataDir, NodeID: cfg.NodeID, Logger: log})
	if err != nil {
		return err
	}
	defer node.Close()

	svc := service.New(service.Options{Config: cfg, Key: key, Store: node, Logger: log})
	ln, err := localapi.Listen(localapi.ListenOptions{
		Path:           *socket,
		AllowedUIDs:    []uint32{0, api.UID},
		MaxConnections: 32,
		Logger:         log,
	})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           localapi.NewHandler(svc, localapi.HandlerOptions{APIUID: api.UID, Logger: log}),
		ConnContext:       localapi.ConnContext,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	v := version.Get()
	log.Info("skep-cluster started", "version", v.Version, "commit", v.Commit, "node_id", cfg.NodeID,
		"pin", key.Pin(), "socket", *socket, "listen_address", cfg.PeerAddress(), "pid", os.Getpid())

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	bg := make(chan struct{})
	go func() { defer close(bg); svc.Run(runCtx) }()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
	case err = <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	}
	shCtx, shCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shCancel()
	_ = srv.Shutdown(shCtx)
	_ = ln.Close()
	cancel()
	<-bg
	log.Info("skep-cluster stopped")
	return err
}
