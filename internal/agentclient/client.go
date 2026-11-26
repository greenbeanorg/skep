// Package agentclient is skep-api's client for the agent's internal API on
// its Unix socket. It implements the backend interface of the public API
// handlers, so those handlers do not know whether they run inside the agent
// or in front of it.
//
// Every call must carry an agentproto.Caller in its context: the agent
// refuses requests without an actor.
package agentclient

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

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Defaults from ADR 0006 decision 1b.
const (
	DefaultConnectTimeout = 5 * time.Second
	DefaultRequestTimeout = 30 * time.Second
	// maxResponseBody bounds what the client reads from the agent.
	maxResponseBody = 16 << 20
)

// ErrUnavailable is returned when the agent cannot be reached. The public
// API answers it with 503 agent_unavailable.
var ErrUnavailable = &models.Error{
	Code:    models.CodeAgentUnavailable,
	Message: "the host agent is not reachable; node operations are unavailable",
}

// Options configures a Client.
type Options struct {
	// SocketPath is the agent's Unix socket.
	SocketPath string
	// ConnectTimeout and RequestTimeout default to the values above.
	ConnectTimeout time.Duration
	RequestTimeout time.Duration
	// Build is the agent build this client accepts; default
	// agentproto.BuildID(), skep-api's own (ADR 0012 answer 9).
	Build string
}

// VersionMismatchError is returned for any response from an agent of
// another build. The public API answers 503 agent_version_mismatch.
type VersionMismatchError struct {
	Agent, API string
}

func (e *VersionMismatchError) Error() string { return e.Model().Message }

// Model is the service error the public API answers with.
func (e *VersionMismatchError) Model() *models.Error {
	agent := e.Agent
	if agent == "" {
		agent = "a build that does not report its version"
	}
	return &models.Error{Code: models.CodeAgentVersionMismatch,
		Message: "skep-agent runs " + agent + " and skep-api runs " + e.API +
			"; they come from one package and must be restarted together (skep-setup upgrade)"}
}

// versionCheck refuses every response from an agent of another build,
// before anything in it is used.
type versionCheck struct {
	next http.RoundTripper
	want string
}

func (v versionCheck) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := v.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if got := resp.Header.Get(agentproto.HeaderAgentVersion); got != v.want {
		resp.Body.Close()
		return nil, &VersionMismatchError{Agent: got, API: v.want}
	}
	return resp, nil
}

// Client talks to one agent.
type Client struct {
	http    *http.Client
	timeout time.Duration
}

// New returns a client for the agent socket. It does not connect; the
// agent may start later or restart at any time.
func New(opts Options) (*Client, error) {
	if opts.SocketPath == "" {
		return nil, errors.New("agentclient: socket path must not be empty")
	}
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = DefaultConnectTimeout
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = DefaultRequestTimeout
	}
	if opts.Build == "" {
		opts.Build = agentproto.BuildID()
	}
	dialer := &net.Dialer{Timeout: opts.ConnectTimeout}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", opts.SocketPath)
		},
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       time.Minute,
		ResponseHeaderTimeout: opts.RequestTimeout,
		// The peer is a local process identified by the kernel; there
		// is no proxy and no TLS on this hop.
		Proxy: nil,
	}
	return &Client{http: &http.Client{Transport: versionCheck{next: transport, want: opts.Build}}, timeout: opts.RequestTimeout}, nil
}

// Close releases idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// AgentNotAnsweringReason is the agent check's error when the agent's
// socket does not answer.
const AgentNotAnsweringReason = "the host agent's socket is not answering: the agent is down, still starting, or refusing to start; see /var/log/daemon.log"

// Health returns the agent's health document. When the agent cannot be
// reached the result is an unavailable document saying so; it never fails.
func (c *Client) Health(ctx context.Context) models.Health {
	var h models.Health
	if err := c.do(ctx, http.MethodGet, "/health", nil, nil, &h); err != nil {
		// Critical: without the agent nothing about the node can be
		// answered.
		h = models.Health{}
		var me *models.Error
		if errors.As(err, &me) && me.Code == models.CodeAgentVersionMismatch {
			// No version strings here: health is public.
			h.Record("agent", true, "skep-agent and skep-api are different builds; restart both (skep-setup upgrade)")
			return h
		}
		// Fixed texts: the cause may hold host paths or the agent's own
		// error, which do not belong in the health document.
		if errors.Is(err, ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) {
			h.Record("agent", true, AgentNotAnsweringReason)
			return h
		}
		h.Record("agent", true, "the host agent is not reachable")
		return h
	}
	h.Record("agent", true, "")
	return h
}

// System returns the system document of the agent's node.
func (c *Client) System(ctx context.Context) (models.System, error) {
	var sys models.System
	err := c.do(ctx, http.MethodGet, "/system", nil, nil, &sys)
	return sys, err
}

// Hardware returns the discovered hardware of a node.
func (c *Client) Hardware(ctx context.Context, nodeID string) (models.Hardware, error) {
	var hw models.Hardware
	err := c.do(ctx, http.MethodGet, "/nodes/"+url.PathEscape(nodeID)+"/hardware", nil, nil, &hw)
	return hw, err
}

// Tasks returns a page of tasks.
func (c *Client) Tasks(ctx context.Context, opts models.TaskListOptions) (models.TaskList, error) {
	q := url.Values{}
	if opts.Status != "" {
		q.Set("status", string(opts.Status))
	}
	if opts.Limit != 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	var list models.TaskList
	err := c.do(ctx, http.MethodGet, "/tasks", q, nil, &list)
	return list, err
}

// Task returns one task with its events.
func (c *Client) Task(ctx context.Context, id string) (models.TaskDetail, error) {
	var detail models.TaskDetail
	err := c.do(ctx, http.MethodGet, "/tasks/"+url.PathEscape(id), nil, nil, &detail)
	return detail, err
}

// TaskByKey returns the task the agent created with an idempotency key
// (step 8d-1, D7); models.ErrNotFound without one. It is not in the public
// API: reconciliation reads the task of a reservation with it.
func (c *Client) TaskByKey(ctx context.Context, key string) (models.Task, error) {
	var task models.Task
	err := c.do(ctx, http.MethodGet, "/tasks/by-key/"+url.PathEscape(key), nil, nil, &task)
	return task, err
}

// CancelTask asks the agent to cancel a task for the caller in ctx and
// returns the task as it is after the request.
func (c *Client) CancelTask(ctx context.Context, id string) (models.Task, error) {
	var task models.Task
	err := c.do(ctx, http.MethodPost, "/tasks/"+url.PathEscape(id)+"/cancel", nil, nil, &task)
	return task, err
}

// do performs one request, with in as its JSON body when not nil, and
// decodes the response into out.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	caller, ok := agentproto.CallerFrom(ctx)
	if !ok {
		return errors.New("agentclient: the request context carries no caller")
	}
	if err := caller.Validate(); err != nil {
		return fmt.Errorf("agentclient: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// The host is a placeholder: the transport always dials the socket.
	u := url.URL{Scheme: "http", Host: "agent", Path: agentproto.Prefix + path, RawQuery: query.Encode()}
	var reqBody io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("agentclient: encode the request: %w", err)
		}
		reqBody = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reqBody)
	if err != nil {
		return fmt.Errorf("agentclient: %w", err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	caller.SetHeaders(req.Header)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		var vm *VersionMismatchError
		if errors.As(err, &vm) {
			return vm.Model()
		}
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) && !isDialError(err) {
			return ctxErr
		}
		return fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	if len(body) > maxResponseBody {
		return errors.New("agentclient: the agent's response is too large")
	}
	if resp.StatusCode/100 != 2 {
		return responseError(resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("agentclient: decode the agent's response: %w", err)
	}
	return nil
}

func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// responseError turns an error envelope from the agent back into the
// service error it came from, so the public API answers as if the agent's
// service layer had been called directly.
func responseError(status int, body []byte) error {
	var env models.ErrorResponse
	if err := json.Unmarshal(body, &env); err != nil || env.Error.Code == "" {
		return fmt.Errorf("agentclient: the agent answered %d without an error envelope", status)
	}
	e := env.Error
	if status == http.StatusNotFound && e.Code == httpkit.CodeNotFound {
		return fmt.Errorf("agent: %w", models.ErrNotFound)
	}
	if want, known := httpkit.StatusForCode[e.Code]; known && want == status {
		return &models.Error{Code: e.Code, Message: e.Message, Details: e.Details}
	}
	// Anything else (an internal error, a refused peer, a protocol
	// mistake) is the API's problem, not the client's.
	return fmt.Errorf("agentclient: the agent answered %d %s: %s", status, e.Code, e.Message)
}
