package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/joinstate"
	"github.com/greenbeanorg/skep/internal/cluster/layout"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/membership"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
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
	// The start rule (decisions D1 and D2 of step 8b-2a): refuse an
	// unfinished wipe or abandon and an earlier build's layout, put a
	// moved-aside one-node state back when that is certain to be right.
	if err := service.StartRule(*dataDir, log); err != nil {
		return err
	}
	stateDir := layout.State(*dataDir)
	// A node that learned it was removed runs only its local socket
	// (decision D11).
	removed, err := service.ReadRemoved(*dataDir)
	if err != nil {
		return fmt.Errorf("the removal record: %w", err)
	}
	if removed != nil {
		svc := service.New(service.Options{Config: cfg, Key: key, DataDir: *dataDir, Removed: removed, Logger: log})
		log.Error("this node was removed from its cluster; only the local socket runs: run \"skep-cluster leave --wipe\"",
			"cluster_id", removed.ClusterID, "how", removed.How, "at", removed.At)
		return serveSocket(ctx, svc, nil, *socket, api.UID, log, cfg, key)
	}
	// The applied-membership file (ADR 0013 amendment 1) is read before
	// the store starts: until the state machine reaches its index, the
	// node accepts keys by it.
	writer, err := membership.NewWriter(stateDir)
	if err != nil {
		return fmt.Errorf("the applied-membership file: %w", err)
	}
	join, err := joinstate.Read(*dataDir)
	if err != nil {
		return fmt.Errorf("the join record: %w", err)
	}
	if join != nil && join.Phase == joinstate.PhaseCalling {
		// The join command stopped while it may have sent the call:
		// its outcome is unknown (ADR 0013 amendment 1, point 3).
		join.Phase = joinstate.PhaseUnknown
		if err := joinstate.Write(*dataDir, *join); err != nil {
			return fmt.Errorf("the join record: %w", err)
		}
	}
	machine := clusterstate.New()
	machine.SetMembershipWriter(writer.Write)
	svc := service.New(service.Options{Config: cfg, Key: key, DataDir: *dataDir, Machine: machine, Writer: writer, Join: join, Logger: log})
	machine.SetOnApplied(svc.OnApplied)
	var trans *raftnet.Transport
	if addr := cfg.PeerAddress(); addr != "" {
		// A node with a listen address runs Raft over TLS on 8444.
		trans, err = raftnet.New(raftnet.Options{LocalAddress: addr, Certificate: key.Certificate, Trust: svc, Peer: svc, Logger: log})
		if err != nil {
			return err
		}
	} else if join != nil {
		return errors.New("this node is joining a cluster but cluster.json has no listen address")
	}
	opts := store.Options{Dir: stateDir, NodeID: cfg.NodeID, Logger: log, Machine: machine, Empty: join != nil}
	if trans != nil {
		opts.Transport = trans
	}
	node, err := store.Open(opts)
	if err != nil {
		return err
	}
	defer node.Close()
	svc.SetStore(node, trans)
	return serveSocket(ctx, svc, node, *socket, api.UID, log, cfg, key)
}

// serveSocket serves the local API and runs the service's background work
// until ctx ends. node is nil on a removed node.
func serveSocket(ctx context.Context, svc *service.Service, node *store.Node, socket string, apiUID uint32, log *slog.Logger, cfg clusterconf.Config, key *nodekey.Key) error {
	if node != nil {
		defer node.Close()
	}
	ln, err := localapi.Listen(localapi.ListenOptions{
		Path:           socket,
		AllowedUIDs:    []uint32{0, apiUID},
		MaxConnections: 32,
		Logger:         log,
	})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           localapi.NewHandler(svc, localapi.HandlerOptions{APIUID: apiUID, Logger: log}),
		ConnContext:       localapi.ConnContext,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	v := version.Get()
	log.Info("skep-cluster started", "version", v.Version, "commit", v.Commit, "node_id", cfg.NodeID,
		"pin", key.Pin(), "socket", socket, "listen_address", cfg.PeerAddress(), "pid", os.Getpid())

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
