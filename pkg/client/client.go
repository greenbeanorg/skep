// Package client is a Go client for the Skep REST API under /api/v1. It is
// what skepctl uses, and it uses nothing but the public API.
//
// Every connection is checked, in one of two ways, during the TLS
// handshake and before anything is sent:
//
//   - against a pinned public key (ADR 0006 decision 6), for skep-api's
//     self-signed certificate; a server that presents another key is
//     refused;
//   - by verifying the certificate chain and host name (Options.VerifyChain)
//     against the system roots or given CA certificates, for an
//     operator-supplied certificate; a pin is then optional.
//
// A Client cannot be created with neither. FetchCertificate is the only
// way to talk to a server without either, and it sends nothing but a
// handshake.
package client

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// Prefix is the path prefix of API version 1.
const Prefix = "/api/v1"

// DefaultTimeout bounds one request when Options.Timeout is zero.
const DefaultTimeout = 30 * time.Second

// maxResponseBytes bounds what is read from one response.
const maxResponseBytes = 16 << 20

// Options configures a Client.
type Options struct {
	// Server is the address of skep-api, in any form NormalizeServer
	// accepts.
	Server string
	// Pin is the SHA-256 of the server's public key, as "sha256:<hex>".
	// Required unless VerifyChain is set; with VerifyChain it is checked
	// in addition when given.
	Pin string
	// VerifyChain verifies the server's certificate chain against RootCAs
	// and its host name, as for any public TLS server. Use it for an
	// operator-supplied certificate.
	VerifyChain bool
	// RootCAs are the CA certificates for VerifyChain. Nil means the
	// system roots.
	RootCAs *x509.CertPool
	// Token is the bearer session token. It may be empty for Login.
	Token string
	// Timeout bounds one request. Zero means DefaultTimeout.
	Timeout time.Duration
	// UserAgent is sent with every request.
	UserAgent string
}

// Client calls the Skep API. It is safe for concurrent use.
type Client struct {
	server    string
	http      *http.Client
	token     string
	userAgent string
}

// New returns a client for a server whose public key is pinned.
func New(opts Options) (*Client, error) {
	server, err := NormalizeServer(opts.Server)
	if err != nil {
		return nil, err
	}
	host, err := hostPort(server)
	if err != nil {
		return nil, err
	}
	hostname, _, _ := net.SplitHostPort(host)
	cfg, err := tlsConfig(hostname, opts.Pin, opts.VerifyChain, opts.RootCAs)
	if err != nil {
		return nil, err
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	transport := &http.Transport{
		// No proxy: the connection goes straight to the server whose key
		// is pinned, the same way FetchCertificate reached it.
		Proxy:               nil,
		TLSClientConfig:     cfg,
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     30 * time.Second,
	}
	return &Client{
		server: server,
		http: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			// The API never redirects; following one could send the
			// token somewhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		token:     opts.Token,
		userAgent: opts.UserAgent,
	}, nil
}

// Server returns the normalized server address.
func (c *Client) Server() string { return c.server }

// Close releases idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// APIError is an error response from the API.
type APIError struct {
	// Status is the HTTP status code.
	Status int
	models.ErrorBody
	// RetryAfter is set when the server sent Retry-After in seconds.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("%s (HTTP %d, %s)", e.Message, e.Status, e.Code)
	if e.RequestID != "" {
		msg += ", request " + e.RequestID
	}
	return msg
}

// IsUnauthenticated reports whether err says that the session is missing,
// expired, or revoked.
func IsUnauthenticated(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusUnauthorized && ae.Code == "unauthenticated"
}

// do sends one request and decodes a JSON response into out (unless out is
// nil). Errors from the API come back as *APIError.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := c.server + Prefix + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// A key mismatch is returned as itself, so callers can tell it
		// from a network problem.
		var km *KeyMismatchError
		if errors.As(err, &km) {
			return km
		}
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return errors.New("the response is too large")
	}
	if resp.StatusCode >= 300 {
		return decodeError(resp, data)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("the server's response is not the expected JSON: %w", err)
	}
	return nil
}

func decodeError(resp *http.Response, data []byte) error {
	ae := &APIError{Status: resp.StatusCode}
	var env models.ErrorResponse
	if err := json.Unmarshal(data, &env); err == nil && env.Error.Code != "" {
		ae.ErrorBody = env.Error
	} else {
		ae.Code = "http_" + strconv.Itoa(resp.StatusCode)
		ae.Message = http.StatusText(resp.StatusCode)
	}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		ae.RetryAfter = time.Duration(s) * time.Second
	}
	return ae
}

// Login starts a bearer session and returns it, with the token. The client
// does not keep the token; create a new Client with it.
func (c *Client) Login(ctx context.Context, username, password string) (models.SessionResponse, error) {
	var out models.SessionResponse
	err := c.do(ctx, http.MethodPost, "/auth/login", nil, models.LoginRequest{
		Username: username, Password: password, Session: models.SessionBearer,
	}, &out)
	if err != nil {
		return models.SessionResponse{}, err
	}
	if out.Token == "" {
		return models.SessionResponse{}, errors.New("the server did not return a session token")
	}
	return out, nil
}

// Logout ends the session of the client's token.
func (c *Client) Logout(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/auth/logout", nil, nil, nil)
}

// Session describes the session of the client's token.
func (c *Client) Session(ctx context.Context) (models.SessionResponse, error) {
	var out models.SessionResponse
	return out, c.do(ctx, http.MethodGet, "/auth/session", nil, nil, &out)
}

// System returns the system document of the node.
func (c *Client) System(ctx context.Context) (models.System, error) {
	var out models.System
	return out, c.do(ctx, http.MethodGet, "/system", nil, nil, &out)
}

// Hardware returns the hardware discovery result of a node.
func (c *Client) Hardware(ctx context.Context, nodeID string) (models.Hardware, error) {
	var out models.Hardware
	return out, c.do(ctx, http.MethodGet, "/nodes/"+url.PathEscape(nodeID)+"/hardware", nil, nil, &out)
}

// Tasks returns one page of tasks, newest first.
func (c *Client) Tasks(ctx context.Context, opts models.TaskListOptions) (models.TaskList, error) {
	q := url.Values{}
	if opts.Status != "" {
		q.Set("status", string(opts.Status))
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	var out models.TaskList
	return out, c.do(ctx, http.MethodGet, "/tasks", q, nil, &out)
}

// Task returns one task with its events.
func (c *Client) Task(ctx context.Context, id string) (models.TaskDetail, error) {
	var out models.TaskDetail
	return out, c.do(ctx, http.MethodGet, "/tasks/"+url.PathEscape(id), nil, nil, &out)
}

// AuditOptions selects a page of the access audit log.
type AuditOptions struct {
	// Limit is the page size. Zero means the server's default.
	Limit int
	// Cursor is the NextCursor of the previous page.
	Cursor string
}

// Audit returns one page of the access audit log, newest first.
func (c *Client) Audit(ctx context.Context, opts AuditOptions) (models.AuditList, error) {
	q := url.Values{}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	var out models.AuditList
	return out, c.do(ctx, http.MethodGet, "/audit", q, nil, &out)
}
