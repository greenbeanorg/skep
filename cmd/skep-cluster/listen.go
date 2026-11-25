package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/store"
)

// runListen is "skep-cluster listen -address ADDR" (ADR 0013 B1): on the
// founding node, before its first admit, it gives the one-node cluster a
// node-to-node address. It writes cluster.json and restarts skep_cluster;
// skep-api's node-to-node listener (api.json, port 8445) comes with step
// 8e.
func runListen(ctx context.Context, args []string, e env) error {
	flags := flag.NewFlagSet("skep-cluster listen", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	var (
		address    = flags.String("address", "", "the management address to listen on (required)")
		configPath = flags.String("config", clusterconf.DefaultPath, "path to cluster.json")
		dataDir    = flags.String("data-dir", clusterconf.DefaultDataDir, "directory of the node key and the cluster store")
		socket     = flags.String("socket", clusterconf.DefaultSocket, "Unix socket of the running service")
		account    = flags.String("account", clusterconf.Account, "the service account")
		noRestart  = flags.Bool("no-restart", false, "write cluster.json and leave the restart of skep_cluster to the administrator")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if e.euid != 0 {
		return errNeedsRoot("listen")
	}
	if *address == "" {
		return errors.New("listen needs -address: there is no default address")
	}
	addr, err := clusterconf.ParseListenAddress(*address)
	if err != nil {
		return err
	}
	if err := checkLocalAddress(e, addr); err != nil {
		return err
	}
	cfg, err := clusterconf.Load(*configPath)
	if err != nil {
		return err
	}
	acct, err := e.lookup(*account)
	if err != nil {
		return fmt.Errorf("the service account: %w", err)
	}

	// Refuse unless the Raft configuration is this node alone. The
	// running service is asked: it holds the store's lock.
	client := localapi.NewClient(*socket)
	rc, err := client.RaftConfiguration(ctx)
	if err != nil {
		return fmt.Errorf("cannot read the Raft configuration from the running service: %w", err)
	}
	if !store.Alone(rc.Servers, cfg.NodeID) {
		return fmt.Errorf("refused: the Raft configuration has %d servers; the listen address can be set this way "+
			"only while this node is alone", len(rc.Servers))
	}

	cfg.ListenAddress = addr.String()
	ownerUID, ownerGID := e.configOwner(acct.GID)
	if err := clusterconf.Write(*configPath, cfg, ownerUID, ownerGID); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "Wrote listen_address %s to %s.\n", cfg.ListenAddress, *configPath)
	if *noRestart {
		fmt.Fprintf(e.stdout, "Restart the service to open %s:\n  service skep_cluster restart\n", cfg.PeerAddress())
		return checkNoRootFiles(e, *dataDir)
	}
	if err := e.restartService(ctx); err != nil {
		return fmt.Errorf("cluster.json is written, but restarting skep_cluster failed: %w", err)
	}
	// The service opens the listener and updates its own member entry
	// and Raft server entry; wait until it says so.
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for {
		st, err := client.Status(ctx)
		switch {
		case err != nil:
			last = err.Error()
		case !st.Listening || st.ListenAddress != cfg.PeerAddress():
			last = "the listener is not open yet"
		case len(st.Members) != 1 || st.Members[0].Address != cfg.PeerAddress():
			last = "the member entry does not carry the address yet"
		default:
			fmt.Fprintf(e.stdout, "skep-cluster listens on %s (TLS 1.3, pinned keys only).\n", cfg.PeerAddress())
			return checkNoRootFiles(e, *dataDir)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("skep_cluster was restarted, but after 30 s %s; see \"skep-cluster health\"", last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
