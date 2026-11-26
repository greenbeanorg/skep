// Package guestapi is what skep-cluster's local API says about the guest
// inventory (step 8d-1, D22–D24): the guests-read and guests-write
// messages, and the client skep-api calls them with. It imports only the
// standard library, clusterstate and pkg/models, so skep-api does not link
// the Raft library or the rest of skep-cluster.
package guestapi

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

// Paths of the two operations on skep-cluster's socket.
const (
	PathGuestsRead  = "/cluster/v1/guests"
	PathGuestsWrite = "/cluster/v1/guests/write"
)

// ActorReconciliation is the actor of the entries skep-api's
// reconciliation writes.
const ActorReconciliation = "reconciliation"

// actorRE is a guest entry's actor: an API user, or reconciliation.
var actorRE = regexp.MustCompile(`^(user:[a-z][a-z0-9._-]{0,31}|reconciliation)$`)

// ValidActor reports whether actor may write a guest entry.
func ValidActor(actor string) bool { return actorRE.MatchString(actor) }

// GuestInfo is one guest of guests-read: the entry, its computed flag and
// its owner's node name.
type GuestInfo struct {
	clusterstate.Guest
	OwnerName string `json:"owner_name"`
}

// ReservationInfo is one reservation of guests-read.
type ReservationInfo struct {
	clusterstate.Reservation
	OwnerName string `json:"owner_name"`
}

// GuestsAnswer is the answer of guests-read (D22): the inventory and the
// four facts users-read gives. A joining or removed node answers no
// guests.
type GuestsAnswer struct {
	ClusterID    string            `json:"cluster_id"`
	GuestsIndex  uint64            `json:"guests_index"`
	Guests       []GuestInfo       `json:"guests"`
	Reservations []ReservationInfo `json:"reservations"`
	LeaderKnown  bool              `json:"leader_known"`
	Stalled      bool              `json:"stalled"`
	Joining      bool              `json:"joining"`
	Removed      bool              `json:"removed"`
}

// The data of each guests-write type: what the caller may say. None has
// an owner (the service sets it from this node, or the leader from the
// caller's key) and none a time (the leader's clock). Decoding is strict:
// a body that names an owner is refused.
type (
	// ReserveData is guest.reserve's data.
	ReserveData struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
		Key  string `json:"key"`
	}
	// ReleaseData is guest.release's data.
	ReleaseData struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
		Key  string `json:"key"`
	}
	// AddData is guest.add's data.
	AddData struct {
		ID     string `json:"id"`
		Kind   string `json:"kind"`
		Name   string `json:"name"`
		TaskID string `json:"task_id"`
	}
	// RemoveData is guest.remove's data.
	RemoveData struct {
		ID string `json:"id"`
	}
)

// WriteRequest is the body of guests-write: one guest entry.
type WriteRequest struct {
	Type      string          `json:"type"`
	OpID      string          `json:"op_id"`
	Actor     string          `json:"actor"`
	RequestID string          `json:"request_id,omitempty"`
	Data      json.RawMessage `json:"data"`
}

// Outcomes of guests-write.
const (
	WriteApplied   = "applied"
	WriteDuplicate = "duplicate"
	WriteUnchanged = "unchanged"
)

// WriteResult is the answer of guests-write.
type WriteResult struct {
	Index     uint64 `json:"index"`
	ClusterID string `json:"cluster_id,omitempty"`
	Outcome   string `json:"outcome"`
}

// strict decodes b into v, refusing unknown fields.
func strict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("more than one JSON value")
	}
	return nil
}

// Entry builds the clusterstate data of a guests-write request with its
// owner and time, which never come from the request (D23). The data is
// decoded strictly.
func Entry(typ string, data json.RawMessage, owner string, now int64) (any, error) {
	switch typ {
	case clusterstate.TypeGuestReserve:
		var d ReserveData
		if err := strict(data, &d); err != nil {
			return nil, err
		}
		return clusterstate.GuestReserve{Kind: d.Kind, Name: d.Name, Key: d.Key, Owner: owner, At: now}, nil
	case clusterstate.TypeGuestRelease:
		var d ReleaseData
		if err := strict(data, &d); err != nil {
			return nil, err
		}
		return clusterstate.GuestRelease{Kind: d.Kind, Name: d.Name, Key: d.Key, Owner: owner}, nil
	case clusterstate.TypeGuestAdd:
		var d AddData
		if err := strict(data, &d); err != nil {
			return nil, err
		}
		return clusterstate.GuestAdd{ID: d.ID, Kind: d.Kind, Name: d.Name, TaskID: d.TaskID, Owner: owner, At: now}, nil
	case clusterstate.TypeGuestRemove:
		var d RemoveData
		if err := strict(data, &d); err != nil {
			return nil, err
		}
		return clusterstate.GuestRemove{ID: d.ID, Owner: owner}, nil
	}
	return nil, errors.New("not a guest entry type")
}

// Client calls guests-read and guests-write on skep-cluster's socket, as
// the API account.
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
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
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

// Read calls guests-read.
func (c *Client) Read(ctx context.Context) (GuestsAnswer, error) {
	var a GuestsAnswer
	err := c.do(ctx, http.MethodGet, PathGuestsRead, nil, &a)
	return a, err
}

// Write calls guests-write.
func (c *Client) Write(ctx context.Context, req WriteRequest) (WriteResult, error) {
	var res WriteResult
	err := c.do(ctx, http.MethodPost, PathGuestsWrite, req, &res)
	return res, err
}
