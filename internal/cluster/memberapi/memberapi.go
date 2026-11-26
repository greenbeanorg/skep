// Package memberapi is what skep-cluster's local API says about members
// (step 8c-2, D21): the admit, admit-withdraw and remove messages, a view
// of the status, and the client skep-api calls them with. Like userapi it
// imports only the standard library, clusterstate and pkg/models, so
// skep-api does not link the Raft library. localapi aliases its types.
package memberapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Paths of the operations on skep-cluster's socket.
const (
	PathHealth   = "/cluster/v1/health"
	PathStatus   = "/cluster/v1/status"
	PathAdmit    = "/cluster/v1/admit"
	PathWithdraw = "/cluster/v1/admit/withdraw"
	PathRemove   = "/cluster/v1/remove"
)

// RemoveTimeout is the socket's limit of the remove operation; skep-api
// waits longer for its answer.
const RemoveTimeout = 60 * time.Second

// MaxRequestLine is the longest join request line skep-api passes on
// (D20 of step 8c-2).
const MaxRequestLine = 2 << 10

// userActorRE is the actor skep-api names for a change it asks for on
// behalf of a user: "user:" and the username (the users-write rule of
// step 8c-1, its "user:" form). A service test keeps the two equal.
var userActorRE = regexp.MustCompile(`^user:[a-z][a-z0-9._-]{0,31}$`)

// ValidUserActor reports whether actor is "user:<username>".
func ValidUserActor(actor string) bool { return userActorRE.MatchString(actor) }

// requestIDRE is a request ID skep-api may pass on: what httpkit makes and
// accepts.
var requestIDRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// ValidRequestID reports whether id is a request ID the socket accepts.
func ValidRequestID(id string) bool { return requestIDRE.MatchString(id) }

// AdmitRequest is the body of the admit operation. Actor and RequestID
// are skep-api's (D22): root sends neither.
type AdmitRequest struct {
	// Request is the line "skep-cluster join-request" printed.
	Request   string `json:"request"`
	Actor     string `json:"actor,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// AdmitResult is the answer of the admit operation.
type AdmitResult struct {
	// Admission is the line to paste into "skep-cluster join".
	Admission string `json:"admission"`
	ClusterID string `json:"cluster_id"`
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	Pin       string `json:"pin"`
	ExpiresAt int64  `json:"expires_at"`
	OpID      string `json:"op_id"`
}

// WithdrawRequest is the body of the admit-withdraw operation.
type WithdrawRequest struct {
	Name      string `json:"name"`
	Actor     string `json:"actor,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// WithdrawResult is its answer.
type WithdrawResult struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
	OpID   string `json:"op_id"`
}

// RemoveRequest is the body of the remove operation.
type RemoveRequest struct {
	Name string `json:"name"`
	// AllowUnanswered removes the member although too few of the
	// remaining voters answered a status round (decision D7c of 8b-2a).
	// Root only (D23 of step 8c-2).
	AllowUnanswered bool `json:"allow_unanswered,omitempty"`
	// ClusterID, when given, must be this cluster's: compared on the
	// node that receives the request and again on the leader (D24).
	// skep-api always sends it.
	ClusterID string `json:"cluster_id,omitempty"`
	Actor     string `json:"actor,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// RemoveResult is its answer: the first entry (removing) is committed.
type RemoveResult struct {
	NodeID    string `json:"node_id"`
	Name      string `json:"name"`
	OpID      string `json:"op_id"`
	ClusterID string `json:"cluster_id"`
	// HandedOver is true when the node was the leader and handed the
	// leadership over first.
	HandedOver bool `json:"handed_over,omitempty"`
	// Unanswered names the remaining voters that did not answer the
	// status round of the removal (only with AllowUnanswered).
	Unanswered []string `json:"unanswered,omitempty"`
}

// JoinStatus is a joining node's part of the status.
type JoinStatus struct {
	Phase     string `json:"phase"`
	ClusterID string `json:"cluster_id"`
	Message   string `json:"message,omitempty"`
}

// RemovedStatus is the status of a node that was removed.
type RemovedStatus struct {
	ClusterID string `json:"cluster_id"`
	At        string `json:"at"`
	How       string `json:"how"`
}

// ClockStatus is one member's clock as this node measured it.
type ClockStatus struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
	// Known is false while there is no sample (Note says why).
	Known bool `json:"known"`
	// OffsetMillis is the member's time minus this node's;
	// UncertaintyMillis is half the round trip.
	OffsetMillis      int64     `json:"offset_ms,omitempty"`
	UncertaintyMillis int64     `json:"uncertainty_ms,omitempty"`
	RoundTripMillis   int64     `json:"round_trip_ms,omitempty"`
	At                time.Time `json:"at,omitempty"`
	Note              string    `json:"note,omitempty"`
}

// View is the part of the status skep-api reads for GET /api/v1/cluster:
// localapi.Status without the Raft library's types. A test keeps its tags
// in line with localapi.Status.
type View struct {
	NodeID        string `json:"node_id"`
	NodeName      string `json:"node_name"`
	ClusterID     string `json:"cluster_id"`
	ListenAddress string `json:"listen_address"`
	Listening     bool   `json:"listening"`
	Raft          struct {
		State    string `json:"state"`
		LeaderID string `json:"leader_id"`
	} `json:"raft"`
	StateMachine struct {
		Stalled *clusterstate.Stall `json:"stalled,omitempty"`
	} `json:"state_machine"`
	Members  []clusterstate.Member  `json:"members"`
	Pendings []clusterstate.Pending `json:"pendings"`
	Servers  []struct {
		ID    string `json:"id"`
		Voter bool   `json:"voter"`
	} `json:"servers,omitempty"`
	Join             *JoinStatus    `json:"join,omitempty"`
	Removed          *RemovedStatus `json:"removed,omitempty"`
	Removals         []string       `json:"removals,omitempty"`
	UnappliedRemoval []string       `json:"unapplied_removal,omitempty"`
	Clocks           []ClockStatus  `json:"clocks,omitempty"`
	Rotations        []string       `json:"rotations,omitempty"`
	Build            string         `json:"build"`
}

// Client calls the member operations, the status and the health on
// skep-cluster's socket, as the API account. It never repeats an
// operation (D25).
type Client struct {
	http *http.Client
}

// NewClient returns a client for the socket. It does not connect.
func NewClient(socket string) *Client {
	return &Client{http: &http.Client{
		// Above the socket's longest limit (remove), so that its answer
		// arrives rather than a client timeout.
		Timeout: RemoveTimeout + 15*time.Second,
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

func (c *Client) do(ctx context.Context, method, path string, in, out any, ok func(int) bool) error {
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
	if !ok(resp.StatusCode) {
		var e models.ErrorResponse
		_ = json.Unmarshal(b, &e)
		return &Error{HTTPStatus: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	return json.Unmarshal(b, out)
}

func is200(s int) bool { return s == http.StatusOK }

// Status returns the view of the service's status.
func (c *Client) Status(ctx context.Context) (View, error) {
	var v View
	err := c.do(ctx, http.MethodGet, PathStatus, nil, &v, is200)
	return v, err
}

// Health returns the service's health document (a 503 is one too).
func (c *Client) Health(ctx context.Context) (models.Health, error) {
	var h models.Health
	err := c.do(ctx, http.MethodGet, PathHealth, nil, &h, func(s int) bool { return s == http.StatusOK || s == http.StatusServiceUnavailable })
	return h, err
}

// Admit admits a host by its request line.
func (c *Client) Admit(ctx context.Context, req AdmitRequest) (AdmitResult, error) {
	var res AdmitResult
	err := c.do(ctx, http.MethodPost, PathAdmit, req, &res, is200)
	return res, err
}

// Withdraw withdraws a pending admission by the host's name.
func (c *Client) Withdraw(ctx context.Context, req WithdrawRequest) (WithdrawResult, error) {
	var res WithdrawResult
	err := c.do(ctx, http.MethodPost, PathWithdraw, req, &res, is200)
	return res, err
}

// Remove removes a member.
func (c *Client) Remove(ctx context.Context, req RemoveRequest) (RemoveResult, error) {
	var res RemoveResult
	err := c.do(ctx, http.MethodPost, PathRemove, req, &res, is200)
	return res, err
}
