// Package peerapi is what skep-cluster's local API tells skep-api for
// forwarding requests between nodes (step 8e-1, D9–D13; ADR 0013 B8): the
// api-key, peers-read and guest-owner messages, the rule that says which
// peer may be forwarded to and accepted from, and the client skep-api
// calls them with. Like guestapi it imports only the standard library,
// clusterstate and pkg/models, so skep-api does not link the Raft library.
package peerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Paths of the three operations on skep-cluster's socket.
const (
	PathAPIKey     = "/cluster/v1/api-key"
	PathPeers      = "/cluster/v1/peers"
	PathGuestOwner = "/cluster/v1/guests/owner"
)

// MaxPeersWait is peers-read's longest wait.
const MaxPeersWait = 5 * time.Second

// APIKeyRequest is the body of api-key (D9): the API pin of this node's
// skep-api and an operation ID (a lower-case UUID). The node is always the
// node of the socket; the body names none.
type APIKeyRequest struct {
	Pin  string `json:"pin"`
	OpID string `json:"op_id"`
}

// Outcomes of api-key.
const (
	APIKeyApplied   = "applied"
	APIKeyDuplicate = "duplicate"
	APIKeyUnchanged = "unchanged"
)

// APIKeyResult is the answer of api-key.
type APIKeyResult struct {
	Index     uint64 `json:"index"`
	ClusterID string `json:"cluster_id"`
	Outcome   string `json:"outcome"`
}

// Clock is a peer's clock offset as the status has it.
type Clock struct {
	Known             bool  `json:"known"`
	OffsetMillis      int64 `json:"offset_ms"`
	UncertaintyMillis int64 `json:"uncertainty_ms"`
}

// Peer is one member entry of peers-read (D11).
type Peer struct {
	NodeID  string `json:"node_id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	State   string `json:"state"`
	APIPin  string `json:"api_pin"`
	// RaftServer is true when the node is a server in the latest Raft
	// configuration.
	RaftServer bool  `json:"raft_server"`
	Clock      Clock `json:"clock"`
}

// PeersAnswer is the answer of peers-read (D11). A node that is joining,
// removed, stalled or not current answers no peers.
type PeersAnswer struct {
	ClusterID string `json:"cluster_id"`
	NodeID    string `json:"node_id"`
	NodeName  string `json:"node_name"`
	// ListenAddress is the cluster's listen address (an IP address, no
	// port), "" on a host without one.
	ListenAddress   string `json:"listen_address"`
	MembershipIndex uint64 `json:"membership_index"`
	LeaderKnown     bool   `json:"leader_known"`
	Stalled         bool   `json:"stalled"`
	Joining         bool   `json:"joining"`
	Removed         bool   `json:"removed"`
	// Current is true only when the accepted keys come from the state
	// machine and the state machine is up to date.
	Current bool   `json:"current"`
	Peers   []Peer `json:"peers"`
}

// Self returns this node's own member entry in the answer.
func (a PeersAnswer) Self() (Peer, bool) {
	return a.Find(a.NodeID)
}

// Find returns the peer with a node ID, or, failing that, a name.
func (a PeersAnswer) Find(ref string) (Peer, bool) {
	if ref == "" {
		return Peer{}, false
	}
	for _, p := range a.Peers {
		if p.NodeID == ref {
			return p, true
		}
	}
	for _, p := range a.Peers {
		if p.Name == ref {
			return p, true
		}
	}
	return Peer{}, false
}

// Usable is D13, the one rule for a peer that may be forwarded to and
// accepted from: state member, an API pin, a server in the latest Raft
// configuration, and not this node.
func Usable(p Peer, self string) bool {
	return p.State == clusterstate.StateMember && p.APIPin != "" && p.RaftServer && p.NodeID != self
}

// WhyNotUsable says why a peer is not usable, for an error message.
func WhyNotUsable(p Peer, self string) string {
	switch {
	case p.NodeID == self:
		return "it is this node"
	case p.State == clusterstate.StateJoining:
		return "it is joining the cluster"
	case p.State == clusterstate.StateRemoving:
		return "it is being removed from the cluster"
	case p.State != clusterstate.StateMember:
		return "it is not a member"
	case p.APIPin == "":
		return "its skep-api has not registered its API key yet"
	case !p.RaftServer:
		return "it is not a server in the Raft configuration"
	}
	return ""
}

// GuestOwnerAnswer is the answer of guest-owner (D12): one guest by its ID
// from this node's state machine, and the facts users-read gives.
type GuestOwnerAnswer struct {
	ClusterID   string `json:"cluster_id"`
	Found       bool   `json:"found"`
	Kind        string `json:"kind,omitempty"`
	Owner       string `json:"owner,omitempty"`
	OwnerName   string `json:"owner_name,omitempty"`
	LeaderKnown bool   `json:"leader_known"`
	Stalled     bool   `json:"stalled"`
	Joining     bool   `json:"joining"`
	Removed     bool   `json:"removed"`
}

// Client calls the three operations on skep-cluster's socket, as the API
// account. It never repeats an operation.
type Client struct {
	http *http.Client
}

// NewClient returns a client for the socket. It does not connect.
func NewClient(socket string) *Client {
	return &Client{http: &http.Client{
		Timeout: 25 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
			},
			DisableKeepAlives: true,
		},
	}}
}

// ErrUnreachable means nothing was sent: the socket could not be reached.
var ErrUnreachable = errors.New("skep-cluster is not reachable on its socket")

// ErrNoAnswer means a request was sent and no answer came.
var ErrNoAnswer = errors.New("skep-cluster did not answer")

// Error is an error answer of skep-cluster.
type Error struct {
	HTTPStatus int
	Code       string
	Message    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("skep-cluster answered %d %s: %s", e.HTTPStatus, e.Code, e.Message)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://skep-cluster"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return fmt.Errorf("%w: %v", ErrUnreachable, err)
		}
		return fmt.Errorf("%w: %v", ErrNoAnswer, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNoAnswer, err)
	}
	if resp.StatusCode != http.StatusOK {
		var e models.ErrorResponse
		_ = json.Unmarshal(b, &e)
		return &Error{HTTPStatus: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	return json.Unmarshal(b, out)
}

// APIKey calls api-key.
func (c *Client) APIKey(ctx context.Context, req APIKeyRequest) (APIKeyResult, error) {
	var res APIKeyResult
	err := c.do(ctx, http.MethodPost, PathAPIKey, req, &res)
	return res, err
}

// Peers calls peers-read: it answers when the membership index differs
// from after, or when wait has passed.
func (c *Client) Peers(ctx context.Context, after uint64, wait time.Duration) (PeersAnswer, error) {
	q := url.Values{}
	q.Set("after", strconv.FormatUint(after, 10))
	q.Set("wait", strconv.Itoa(int(wait/time.Second)))
	var a PeersAnswer
	err := c.do(ctx, http.MethodGet, PathPeers+"?"+q.Encode(), nil, &a)
	return a, err
}

// GuestOwner calls guest-owner for one guest ID.
func (c *Client) GuestOwner(ctx context.Context, id string) (GuestOwnerAnswer, error) {
	var a GuestOwnerAnswer
	err := c.do(ctx, http.MethodGet, PathGuestOwner+"?id="+url.QueryEscape(id), nil, &a)
	return a, err
}
