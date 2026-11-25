package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// Client calls the local API on the service's socket. The skep-cluster
// commands use it; skep-api will from step 8c.
type Client struct {
	http *http.Client
}

// NewClient returns a client for the socket. It does not connect.
func NewClient(socket string) *Client {
	return &Client{http: &http.Client{
		// Above the longest operation's limit (RemoveTimeout), so that
		// the service's answer arrives rather than a client timeout.
		Timeout: WriteTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
			},
			DisableKeepAlives: true,
		},
	}}
}

// StatusError is an error answer of the local API.
type StatusError struct {
	HTTPStatus int
	Code       string
	Message    string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("skep-cluster answered %d %s: %s", e.HTTPStatus, e.Code, e.Message)
}

// ErrUnavailable means the socket could not be reached or the connection
// was closed: the service is not running, or this user is not an allowed
// peer.
var ErrUnavailable = errors.New("skep-cluster is not reachable on its socket (is the service running, and are you root?)")

func (c *Client) get(ctx context.Context, path string, okStatus func(int) bool, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://skep-cluster"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if !okStatus(resp.StatusCode) {
		var e models.ErrorResponse
		_ = json.Unmarshal(body, &e)
		return &StatusError{HTTPStatus: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	return json.Unmarshal(body, out)
}

func is200(s int) bool { return s == http.StatusOK }

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://skep-cluster"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		var e models.ErrorResponse
		_ = json.Unmarshal(b, &e)
		return &StatusError{HTTPStatus: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	return json.Unmarshal(b, out)
}

// Admit admits a host by its request line (root only).
func (c *Client) Admit(ctx context.Context, request string) (AdmitResult, error) {
	var res AdmitResult
	err := c.post(ctx, PathAdmit, AdmitRequest{Request: request}, &res)
	return res, err
}

// Withdraw withdraws a pending admission by the host's name (root only).
func (c *Client) Withdraw(ctx context.Context, name string) (WithdrawResult, error) {
	var res WithdrawResult
	err := c.post(ctx, PathWithdraw, WithdrawRequest{Name: name}, &res)
	return res, err
}

// Remove calls the remove operation.
func (c *Client) Remove(ctx context.Context, req RemoveRequest) (RemoveResult, error) {
	var res RemoveResult
	err := c.post(ctx, PathRemove, req, &res)
	return res, err
}

// Health returns the service's health document. A 503 answer is a health
// document too.
func (c *Client) Health(ctx context.Context) (models.Health, error) {
	var h models.Health
	err := c.get(ctx, PathHealth, func(s int) bool { return s == http.StatusOK || s == http.StatusServiceUnavailable }, &h)
	return h, err
}

// Status returns the service's status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	err := c.get(ctx, PathStatus, is200, &s)
	return s, err
}

// RaftConfiguration returns the Raft configuration (root only).
func (c *Client) RaftConfiguration(ctx context.Context) (RaftConfiguration, error) {
	var rc RaftConfiguration
	err := c.get(ctx, PathRaftConfiguration, is200, &rc)
	return rc, err
}
