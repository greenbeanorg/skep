// Package userapi is what skep-cluster's local API says about the
// replicated users (step 8c-1, D11): the users-read and users-write
// messages, and the client skep-api calls them with. It imports only the
// standard library and clusterstate, so skep-api does not link the Raft
// library.
package userapi

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

// Paths of the two operations on skep-cluster's socket.
const (
	PathUsersRead  = "/cluster/v1/users"
	PathUsersWrite = "/cluster/v1/users/write"
)

// MaxUsersWait is the longest wait of users-read (D11 of step 8c-1).
const MaxUsersWait = 5 * time.Second

// UsersAnswer is the answer of users-read: the users with their hashes and
// credential epochs, for skep-api's read copy, and what skep-api needs to
// tell whether there is quorum. A joining or removed node answers no
// users.
type UsersAnswer struct {
	ClusterID string `json:"cluster_id"`
	// UsersIndex is the index of the last entry that changed the users.
	UsersIndex  uint64              `json:"users_index"`
	Users       []clusterstate.User `json:"users"`
	LeaderKnown bool                `json:"leader_known"`
	Stalled     bool                `json:"stalled"`
	Joining     bool                `json:"joining"`
	Removed     bool                `json:"removed"`
}

// UsersWriteRequest is the body of users-write: one user entry. The
// service builds the entry; its time is the leader's.
type UsersWriteRequest struct {
	Type      string          `json:"type"`
	OpID      string          `json:"op_id"`
	Actor     string          `json:"actor"`
	RequestID string          `json:"request_id,omitempty"`
	Data      json.RawMessage `json:"data"`
}

// Outcomes of users-write.
const (
	WriteApplied   = "applied"
	WriteDuplicate = "duplicate"
)

// UserInfo is a user without its password hash.
type UserInfo struct {
	ID                string `json:"id"`
	Username          string `json:"username"`
	DisplayName       string `json:"display_name"`
	RoleID            string `json:"role_id"`
	Disabled          bool   `json:"disabled"`
	CredentialEpoch   uint64 `json:"credential_epoch"`
	PasswordChangedAt int64  `json:"password_changed_at"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
}

// InfoOf drops the hash.
func InfoOf(u clusterstate.User) UserInfo {
	return UserInfo{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, RoleID: u.RoleID, Disabled: u.Disabled,
		CredentialEpoch: u.CredentialEpoch, PasswordChangedAt: u.PasswordChangedAt, CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt}
}

// UsersWriteResult is the answer of users-write: the index of the entry
// that applied the operation (the first one, for a duplicate) and the
// resulting user (none after a delete or an import).
type UsersWriteResult struct {
	Index   uint64    `json:"index"`
	Outcome string    `json:"outcome"`
	User    *UserInfo `json:"user,omitempty"`
}

// Client calls users-read and users-write on skep-cluster's socket, as
// the API account.
type Client struct {
	http *http.Client
}

// NewClient returns a client for the socket. It does not connect.
func NewClient(socket string) *Client {
	return &Client{http: &http.Client{
		Timeout: MaxUsersWait + 20*time.Second,
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
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
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

// UsersRead calls users-read: it waits up to wait for the users index to
// differ from after.
func (c *Client) UsersRead(ctx context.Context, after uint64, wait time.Duration) (UsersAnswer, error) {
	q := url.Values{}
	q.Set("after", strconv.FormatUint(after, 10))
	q.Set("wait", strconv.Itoa(int(wait/time.Second)))
	var a UsersAnswer
	err := c.do(ctx, http.MethodGet, PathUsersRead+"?"+q.Encode(), nil, &a)
	return a, err
}

// UsersWrite calls users-write.
func (c *Client) UsersWrite(ctx context.Context, req UsersWriteRequest) (UsersWriteResult, error) {
	var res UsersWriteResult
	err := c.do(ctx, http.MethodPost, PathUsersWrite, req, &res)
	return res, err
}
