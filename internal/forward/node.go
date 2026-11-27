package forward

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/peerapi"
	"github.com/greenbeanorg/skep/internal/ids"
)

// Timings of the node's own work (D2, D15, D16).
const (
	registerEvery = 10 * time.Second
	bindRetry     = 10 * time.Second
	renewEvery    = 24 * time.Hour
	stepEvery     = time.Second
)

// CheckName is skep-api's health check of forwarding (D45), not critical.
const CheckName = "node_forwarding"

// Cluster is what the node needs from skep-cluster's socket
// (peerapi.Client).
type Cluster interface {
	PeersSource
	APIKey(ctx context.Context, req peerapi.APIKeyRequest) (peerapi.APIKeyResult, error)
	GuestOwner(ctx context.Context, id string) (peerapi.GuestOwnerAnswer, error)
}

// NodeOptions configures the node's forwarding.
type NodeOptions struct {
	// KeyDir is <API state directory>/nodekey: directory 0700, node.key
	// 0600, node.crt, owned by skep-api's account (D1).
	KeyDir  string
	Cluster Cluster
	Users   UserLookup
	// Port is the node listener's port; 0 is Port.
	Port   int
	Now    func() time.Time
	Logger *slog.Logger
}

// Node is skep-api's forwarding state: the API node key, the peers copy,
// the registration of the key, the node listener, the receiver and the
// client.
type Node struct {
	o      NodeOptions
	log    *slog.Logger
	now    func() time.Time
	peers  *Peers
	recv   *Receiver
	client *Client

	mu        sync.Mutex
	key       *nodekey.Key
	keyErr    error
	handler   http.Handler
	listener  *NodeListener
	listenErr error
	nextBind  time.Time
	nextReg   time.Time
	regErr    string
	lastRenew time.Time
}

// NewNode returns the node's forwarding. SetHandler and Run complete it.
func NewNode(o NodeOptions) *Node {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Port == 0 {
		o.Port = Port
	}
	n := &Node{o: o, log: o.Logger, now: o.Now}
	n.peers = NewPeers(o.Cluster, o.Logger, o.Now)
	n.recv = NewReceiver(n.peers, o.Users, o.Now)
	n.client = NewClient(n, n.peers, o.Now)
	n.client.port = o.Port
	return n
}

// Peers is the peers copy.
func (n *Node) Peers() *Peers { return n.peers }

// Receiver makes the receiver's checks.
func (n *Node) Receiver() *Receiver { return n.recv }

// Client sends forwarded requests.
func (n *Node) Client() *Client { return n.client }

// PeersNow returns the last peers-read answer and whether it is fresh.
func (n *Node) PeersNow() (peerapi.PeersAnswer, bool) { return n.peers.Current() }

// GuestOwner asks skep-cluster which node owns a guest.
func (n *Node) GuestOwner(ctx context.Context, id string) (peerapi.GuestOwnerAnswer, error) {
	return n.o.Cluster.GuestOwner(ctx, id)
}

// Forward sends a request to a usable peer (Client.Forward).
func (n *Node) Forward(ctx context.Context, p peerapi.Peer, req Request) Result {
	return n.client.Forward(ctx, p, req)
}

// Key implements Identity: the API node key, or nil.
func (n *Node) Key() *nodekey.Key {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.key
}

// SetHandler sets the node listener's handler (api.NewNodeHandler).
func (n *Node) SetHandler(h http.Handler) {
	n.mu.Lock()
	n.handler = h
	n.mu.Unlock()
}

// Run keeps the peers copy, the key, the registration and the listener
// until ctx ends; the listener is closed then.
func (n *Node) Run(ctx context.Context) {
	done := make(chan struct{})
	go func() { defer close(done); n.peers.Run(ctx) }()
	t := time.NewTicker(stepEvery)
	defer t.Stop()
	for {
		changed := n.peers.Changed()
		n.Step(ctx)
		select {
		case <-ctx.Done():
			n.closeListener()
			<-done
			return
		case <-changed:
		case <-t.C:
		}
	}
}

// Step does one round of the work from the last fresh answer.
func (n *Node) Step(ctx context.Context) {
	a, fresh := n.peers.Current()
	if !fresh {
		return
	}
	if a.ListenAddress == "" {
		// A host without a listen address opens no port and creates no
		// key.
		n.closeListener()
		return
	}
	key := n.ensureKey(a.NodeID)
	if key == nil {
		return
	}
	n.register(ctx, a, key)
	n.ensureListener(a.ListenAddress, key)
}

// ensureKey loads or creates the API node key (D2). It never replaces a
// key: a key file that cannot be used is a health warning and nothing is
// deleted.
func (n *Node) ensureKey(nodeID string) *nodekey.Key {
	n.mu.Lock()
	key, due := n.key, n.lastRenew.IsZero() || n.now().Sub(n.lastRenew) >= renewEvery
	n.mu.Unlock()
	if key != nil {
		if due {
			n.renew(key, nodeID)
		}
		return key
	}
	var err error
	var repaired error
	if err = os.MkdirAll(n.o.KeyDir, 0o700); err == nil {
		err = os.Chmod(n.o.KeyDir, 0o700)
	}
	if err == nil {
		if nodekey.Exists(n.o.KeyDir) {
			key, repaired, err = nodekey.LoadRepairPrefix(n.o.KeyDir, nodekey.PrefixAPI, nodeID, n.now())
		} else {
			key, err = nodekey.CreatePrefix(n.o.KeyDir, nodekey.PrefixAPI, nodeID, n.now())
			if err == nil {
				n.log.Info("created the API node key for forwarding between nodes", "dir", n.o.KeyDir, "pin", key.Pin())
			}
		}
	}
	if repaired != nil {
		n.log.Warn("the API node key's certificate was issued again with the same key", "reason", repaired)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err != nil {
		if n.keyErr == nil || n.keyErr.Error() != err.Error() {
			n.log.Error("cannot use the API node key; forwarding stays off and nothing is deleted", "dir", n.o.KeyDir, "error", err)
		}
		n.keyErr = err
		return nil
	}
	n.key, n.keyErr, n.lastRenew = key, nil, n.now()
	return key
}

func (n *Node) renew(key *nodekey.Key, nodeID string) {
	renewed, err := key.RenewIfDue(nodeID, n.now())
	n.mu.Lock()
	n.lastRenew = n.now()
	n.mu.Unlock()
	if err != nil {
		n.log.Error("cannot renew the API node key's certificate", "error", err)
	} else if renewed {
		n.log.Info("API node key's certificate renewed with the same key", "not_after", key.NotAfter())
	}
}

// register asks skep-cluster to set this node's API pin (D15), at most
// once every 10 s, while the answer says this node is a member whose API
// pin is not the key's.
func (n *Node) register(ctx context.Context, a peerapi.PeersAnswer, key *nodekey.Key) {
	self, ok := a.Self()
	if a.Joining || a.Removed || a.Stalled || !a.Current || !ok || self.State != clusterstate.StateMember || self.APIPin == key.Pin() {
		return
	}
	n.mu.Lock()
	due := !n.now().Before(n.nextReg)
	if due {
		n.nextReg = n.now().Add(registerEvery)
	}
	n.mu.Unlock()
	if !due {
		return
	}
	op, err := ids.NewUUID()
	if err != nil {
		return
	}
	res, err := n.o.Cluster.APIKey(ctx, peerapi.APIKeyRequest{Pin: key.Pin(), OpID: op})
	n.mu.Lock()
	defer n.mu.Unlock()
	if err != nil {
		if err.Error() != n.regErr {
			n.log.Warn("the API node key could not be registered yet; retrying", "error", err)
		}
		n.regErr = err.Error()
		return
	}
	n.regErr = ""
	n.log.Info("the API node key is registered in the membership", "pin", key.Pin(), "index", res.Index, "outcome", res.Outcome)
}

// ensureListener opens, moves or keeps the node listener (D16).
func (n *Node) ensureListener(listen string, key *nodekey.Key) {
	want, err := nodeAddr(listen, n.o.Port)
	n.mu.Lock()
	l, h := n.listener, n.handler
	if l != nil && (err != nil || l.Addr() != want) {
		n.listener = nil
	}
	n.mu.Unlock()
	if l != nil {
		if err == nil && l.Addr() == want {
			return
		}
		_ = l.Close()
		n.log.Info("node listener closed: the listen address changed", "address", l.Addr().String())
	}
	n.mu.Lock()
	if err == nil && n.now().Before(n.nextBind) {
		n.mu.Unlock()
		return
	}
	n.mu.Unlock()
	if err == nil && h == nil {
		err = errors.New("no handler")
	}
	var nl *NodeListener
	if err == nil {
		nl, err = ListenNode(want, func() *tls.Certificate { return key.Certificate() }, n.peers.AcceptPin, h, n.now, n.log)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err != nil {
		if n.listenErr == nil || n.listenErr.Error() != err.Error() {
			n.log.Error("cannot open the node listener; retrying every 10 s", "address", want.String(), "error", err)
		}
		n.listenErr, n.nextBind = err, n.now().Add(bindRetry)
		return
	}
	n.listener, n.listenErr = nl, nil
	n.log.Info("node listener open (forwarding between nodes)", "address", nl.BoundAddr())
}

func (n *Node) closeListener() {
	n.mu.Lock()
	l := n.listener
	n.listener, n.listenErr = nil, nil
	n.mu.Unlock()
	if l != nil {
		_ = l.Close()
		n.log.Info("node listener closed", "address", l.Addr().String())
	}
}

// ListenerAddr is the node listener's bound address, or "".
func (n *Node) ListenerAddr() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.listener == nil {
		return ""
	}
	return n.listener.BoundAddr()
}

// Health is the node_forwarding check (D45): empty on a host without a
// listen address (or before skep-cluster has ever answered).
func (n *Node) Health(context.Context) string {
	a, _ := n.peers.Current()
	if a.ListenAddress == "" {
		return ""
	}
	var out []string
	if p := n.peers.Problem(); p != "" {
		out = append(out, p+": nothing is forwarded and no node is accepted")
	}
	n.mu.Lock()
	key, keyErr, l, listenErr, regErr := n.key, n.keyErr, n.listener, n.listenErr, n.regErr
	n.mu.Unlock()
	switch {
	case keyErr != nil:
		out = append(out, "the API node key cannot be used: "+keyErr.Error())
	case key == nil:
		out = append(out, "the API node key does not exist yet")
	default:
		if self, ok := a.Self(); ok && self.APIPin != key.Pin() {
			msg := "the API node key is not registered in the membership yet"
			if regErr != "" {
				msg += " (last answer: " + regErr + ")"
			}
			out = append(out, msg)
		}
	}
	if l == nil {
		msg := fmt.Sprintf("the node listener on %s port %d is not open", a.ListenAddress, n.o.Port)
		if listenErr != nil {
			msg += ": " + listenErr.Error()
		}
		out = append(out, msg)
	}
	return strings.Join(out, "; ")
}
