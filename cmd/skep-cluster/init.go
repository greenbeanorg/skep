package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/store"
	"github.com/greenbeanorg/skep/internal/ids"
)

// initActor is the actor "skep-cluster init" names on the agent's socket.
const initActor = "skep-cluster-init"

// agentNodeID reads the node ID from skep-agent's read-only system
// operation. Only the root-run init command calls it; the service never
// connects to the agent (ADR 0013 A3, B1).
func agentNodeID(ctx context.Context, socket string) (string, error) {
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
		}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent"+agentproto.Prefix+"/system", nil)
	if err != nil {
		return "", err
	}
	agentproto.Caller{ActorID: initActor, ActorName: initActor}.SetHeaders(req.Header)
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach skep-agent on %s (is skep_agent running?): %w", socket, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("skep-agent answered %s to the system read", resp.Status)
	}
	var sys struct {
		NodeID string `json:"node_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&sys); err != nil {
		return "", fmt.Errorf("skep-agent's system answer: %w", err)
	}
	return sys.NodeID, nil
}

// checkLocalAddress refuses an address that is not assigned to this host:
// the listener could never bind it.
func checkLocalAddress(e env, addr netip.Addr) error {
	local, err := e.localAddrs()
	if err != nil {
		return fmt.Errorf("cannot list this host's addresses: %w", err)
	}
	if !slices.Contains(local, addr) {
		return fmt.Errorf("%s is not an address of this host", addr)
	}
	return nil
}

// prepareDataDir makes the data directory belong to the service account
// with mode 0700, as root, before root is given up. Nothing else in it is
// ever created by root.
func prepareDataDir(e env, dir string, uid, gid uint32) error {
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return err
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
		if err := e.chown(dir, int(uid), int(gid)); err != nil {
			return fmt.Errorf("cannot give %s to the service account: %w", dir, err)
		}
		return os.Chmod(dir, 0o700)
	case err != nil:
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", dir)
	}
	owner, ok := e.owner(info)
	if !ok || owner != uid {
		return fmt.Errorf("%s does not belong to the service account (uid %d); nothing was changed", dir, uid)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s has mode %04o, not 0700; nothing was changed", dir, info.Mode().Perm())
	}
	return nil
}

func runInit(ctx context.Context, args []string, e env) error {
	flags := flag.NewFlagSet("skep-cluster init", flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	var (
		address     = flags.String("address", "", "management address for the node-to-node listener; without it no network listener is opened")
		name        = flags.String("name", "", "node name (default: the host name)")
		configPath  = flags.String("config", clusterconf.DefaultPath, "path to cluster.json")
		dataDir     = flags.String("data-dir", clusterconf.DefaultDataDir, "directory of the node key and the cluster store")
		agentSocket = flags.String("agent-socket", clusterconf.DefaultAgentSocket, "Unix socket of skep-agent")
		account     = flags.String("account", clusterconf.Account, "the service account")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if e.euid != 0 {
		return errNeedsRoot("init")
	}

	// Part 1, as root: everything that needs root, and nothing in the
	// data directory but the directory itself.
	if store.Exists(*dataDir) {
		return fmt.Errorf("%s already holds a cluster store: this host is initialised (see \"skep-cluster status\")", *dataDir)
	}
	acct, err := e.lookup(*account)
	if err != nil {
		return fmt.Errorf("the service account: %w (the skep package creates it)", err)
	}
	if acct.UID == 0 {
		return errors.New("the service account must not be root")
	}
	cfg := clusterconf.Config{NodeName: *name}
	if cfg.NodeName == "" {
		if cfg.NodeName, err = e.hostname(); err != nil {
			return fmt.Errorf("cannot read the host name: %w", err)
		}
	}
	if *address != "" {
		addr, err := clusterconf.ParseListenAddress(*address)
		if err != nil {
			return err
		}
		if err := checkLocalAddress(e, addr); err != nil {
			return err
		}
		cfg.ListenAddress = addr.String()
	}
	if cfg.NodeID, err = e.agentNodeID(ctx, *agentSocket); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := prepareDataDir(e, *dataDir, acct.UID, acct.GID); err != nil {
		return err
	}
	ownerUID, ownerGID := e.configOwner(acct.GID)
	if err := clusterconf.Write(*configPath, cfg, ownerUID, ownerGID); err != nil {
		return err
	}

	// Part 2, as the service account, for good: the key and the store.
	if err := e.drop(acct.UID, acct.GID); err != nil {
		return fmt.Errorf("cannot give up root for the service account: %w", err)
	}
	clusterID, pin, err := createCluster(ctx, *dataDir, cfg, time.Now())
	if err != nil {
		return err
	}
	if rootOwned, err := store.RootOwned(*dataDir, e.owner); err != nil {
		return err
	} else if len(rootOwned) > 0 {
		return fmt.Errorf("root-owned files were left in %s: %v", *dataDir, rootOwned)
	}

	fmt.Fprintf(e.stdout, "This host is now a one-node cluster.\n")
	fmt.Fprintf(e.stdout, "  Node ID:      %s\n  Node name:    %s\n  Cluster ID:   %s\n  Node key pin: %s\n",
		cfg.NodeID, cfg.NodeName, clusterID, pin)
	if cfg.ListenAddress == "" {
		fmt.Fprintf(e.stdout, "  Listener:     none (a single host opens no network port)\n")
	} else {
		fmt.Fprintf(e.stdout, "  Listener:     %s, once the service runs\n", cfg.PeerAddress())
	}
	fmt.Fprintf(e.stdout, "Enable and start the service:\n  sysrc skep_cluster_enable=YES\n  service skep_cluster start\n")
	return nil
}

// createCluster makes the node key (or takes the one an interrupted init
// left) and a store with this node as the only member. It runs as the
// service account.
func createCluster(ctx context.Context, dataDir string, cfg clusterconf.Config, now time.Time) (clusterID, pin string, err error) {
	var key *nodekey.Key
	if nodekey.Exists(dataDir) {
		// An earlier init stopped after the key and before the
		// store. The key is not replaced; its certificate is.
		if key, _, err = nodekey.LoadRepair(dataDir, cfg.NodeID, now); err != nil {
			return "", "", fmt.Errorf("%w (left by an interrupted init; remove %s and %s as root to start over)",
				err, nodekey.KeyFile, nodekey.CertFile)
		}
	} else if key, err = nodekey.Create(dataDir, cfg.NodeID, now); err != nil {
		return "", "", err
	}
	if clusterID, err = ids.NewUUID(); err != nil {
		return "", "", err
	}
	opID, err := ids.NewUUID()
	if err != nil {
		return "", "", err
	}
	entry, err := clusterstate.NewEntry(clusterstate.TypeClusterInit, opID, clusterstate.ActorSystem, cfg.NodeID,
		clusterstate.ClusterInit{ClusterID: clusterID, Member: clusterstate.Member{
			ID: cfg.NodeID, Name: cfg.NodeName, Address: cfg.PeerAddress(), Pin: key.Pin(), State: clusterstate.StateMember,
		}})
	if err != nil {
		return "", "", err
	}
	opts := store.Options{Dir: dataDir, NodeID: cfg.NodeID}
	fail := func(err error) (string, string, error) {
		// Leave no half-made store: the next init starts clean.
		_ = os.Remove(filepath.Join(dataDir, store.DBFile))
		_ = os.RemoveAll(filepath.Join(dataDir, "snapshots"))
		return "", "", err
	}
	if err := store.Bootstrap(opts, cfg.PeerAddress()); err != nil {
		return fail(err)
	}
	node, err := store.Open(opts)
	if err != nil {
		return fail(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := node.WaitLeader(wctx); err != nil {
		node.Close()
		return fail(err)
	}
	if _, err := node.Apply(entry); err != nil {
		node.Close()
		return fail(fmt.Errorf("writing the first entry: %w", err))
	}
	if err := node.Close(); err != nil {
		return fail(err)
	}
	return clusterID, key.Pin(), nil
}
