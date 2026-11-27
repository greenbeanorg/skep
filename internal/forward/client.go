package forward

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/peerapi"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Port is the node listener's TCP port, next to clusterconf.Port (8444).
const Port = 8445

// Client limits (D34, D35).
const (
	dialTimeout    = 5 * time.Second
	forwardTimeout = 30 * time.Second
	// MaxAnswer bounds the answer read from the receiver.
	MaxAnswer = 16 << 20
	// retryPause is the pause between attempts; retryFor bounds the
	// repeats of behind, user_unknown and before_start.
	retryPause = 500 * time.Millisecond
	retryFor   = 5 * time.Second
	// MaxClockSkew is D33's limit: a peer whose clock offset, less its
	// uncertainty, is above it is not forwarded to.
	MaxClockSkew = 15 * time.Second
)

// Identity is what the client signs with: the API node key and its node.
type Identity interface {
	// Key returns the API node key, or nil while there is none.
	Key() *nodekey.Key
}

// Request is a request to forward: what the origin read from its client.
type Request struct {
	Method string
	// URI is the path with its query string, exactly as sent.
	URI            string
	Body           []byte
	ContentType    string
	IdempotencyKey string
	RequestID      string
	SourceIP       string
	// User is the origin's copy of the user.
	UserID string
	Epoch  uint64
}

// Result is the outcome of a forward. Either Status is set (a handler's
// answer, passed through), or Err is the public error to answer.
type Result struct {
	Status int
	Header http.Header
	Body   []byte
	Err    *models.Error
	// Refusal is the receiver's last refusal code, if any; Attempts how
	// many requests were sent or tried.
	Refusal  string
	Attempts int
}

// HTTPStatus is the status to answer: the receiver's, or 503 for Err.
func (r Result) HTTPStatus() int {
	if r.Err != nil {
		if r.Err.Code == auth.CodeUnauthenticated {
			return http.StatusUnauthorized
		}
		return http.StatusServiceUnavailable
	}
	return r.Status
}

// Client sends forwarded requests (D34, D35). Every forward is a new
// connection and a new handshake against the membership as it is then.
type Client struct {
	id    Identity
	peers *Peers
	now   func() time.Time
	// pause is retryPause; tests shorten it.
	pause time.Duration
	// dial is the TCP dialler; tests replace it.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// port is Port; tests use another.
	port int
}

// NewClient returns the client.
func NewClient(id Identity, peers *Peers, now func() time.Time) *Client {
	if now == nil {
		now = time.Now
	}
	return &Client{id: id, peers: peers, now: now, pause: retryPause, port: Port,
		dial: (&net.Dialer{Timeout: dialTimeout}).DialContext}
}

// notSentError is an error after which the receiver cannot have parsed
// the request: the dial or the handshake failed, or the receiver refused
// this node's key (F8: TLS 1.3 reports that at the first read).
type notSentError struct{ err error }

func (e *notSentError) Error() string { return e.err.Error() }
func (e *notSentError) Unwrap() error { return e.err }

// refusedKey reports whether err is the receiver refusing this node's
// certificate in its handshake (facts F8).
func refusedKey(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "remote error" && strings.Contains(op.Err.Error(), "bad certificate")
}

func nodeDetails(p peerapi.Peer, outcome string) map[string]any {
	return map[string]any{"node_id": p.NodeID, "node_name": p.Name, "outcome": outcome}
}

func publicError(code string, p peerapi.Peer, outcome, msg string) *models.Error {
	return &models.Error{Code: code, Message: msg, Details: nodeDetails(p, outcome)}
}

// Unreachable is the public node_unreachable error for a peer, nothing
// sent.
func Unreachable(p peerapi.Peer, why string) *models.Error {
	return publicError(models.CodeNodeUnreachable, p, auth.OutcomeNotWritten,
		fmt.Sprintf("node %s cannot be reached now: %s; nothing was sent", p.Name, why))
}

// ClockSkew is D33's check: the public error when the peer's clock is too
// far from this node's, or nil.
func ClockSkew(p peerapi.Peer) *models.Error {
	off := p.Clock.OffsetMillis
	if off < 0 {
		off = -off
	}
	if p.Clock.Known && time.Duration(off-p.Clock.UncertaintyMillis)*time.Millisecond > MaxClockSkew {
		return publicError(models.CodeNodeClockSkew, p, auth.OutcomeNotWritten,
			fmt.Sprintf("the clock of node %s is %d ms away from this node's (more than 15 s); nothing was sent", p.Name, p.Clock.OffsetMillis))
	}
	return nil
}

// Forward sends req to the peer's node listener and maps the outcome
// (D35). The caller has checked that the peer is usable and its clock.
func (c *Client) Forward(ctx context.Context, p peerapi.Peer, req Request) Result {
	ctx, cancel := context.WithTimeout(ctx, forwardTimeout)
	defer cancel()
	var res Result
	sentOnce := false
	deadline := c.now().Add(retryFor)
	for {
		res.Attempts++
		status, header, body, err := c.send(ctx, p, req)
		switch {
		case err != nil:
			var ns *notSentError
			if errors.As(err, &ns) && !sentOnce {
				sentOnce = true // one more attempt after 500 ms
				if c.sleep(ctx) {
					continue
				}
			}
			if errors.As(err, &ns) {
				res.Err = publicError(models.CodeNodeUnreachable, p, auth.OutcomeNotWritten,
					fmt.Sprintf("node %s cannot be reached (%s); nothing was sent", p.Name, ns.err))
			} else {
				res.Err = publicError(models.CodeNodeUnreachable, p, auth.OutcomeUnknown,
					fmt.Sprintf("the request was sent to node %s and no complete answer came; whether it was carried out is unknown (check the task list on that node)", p.Name))
			}
			return res
		case header.Get(RefusalHeader) == "":
			res.Status, res.Header, res.Body = status, header, body
			return res
		}
		code := header.Get(RefusalHeader)
		res.Refusal = code
		switch code {
		case CodeBehind, CodeUserUnknown, CodeBeforeStart:
			if c.now().Add(c.pause).Before(deadline) && c.sleep(ctx) {
				continue // a new envelope: new nonce and times
			}
			if code == CodeBeforeStart {
				res.Err = publicError(models.CodeNodeUnreachable, p, auth.OutcomeNotWritten,
					fmt.Sprintf("node %s has just restarted its skep-api and does not accept this request yet; nothing was written; try again", p.Name))
			} else {
				res.Err = publicError(models.CodeNodeBehind, p, auth.OutcomeNotWritten,
					fmt.Sprintf("node %s has not yet caught up with the latest change of your account; nothing was written; try again shortly", p.Name))
			}
		case CodeStaleOrigin:
			res.Err = &models.Error{Code: auth.CodeUnauthenticated, Message: "authentication required: log in"}
		case CodeClock:
			res.Err = publicError(models.CodeNodeClockSkew, p, auth.OutcomeNotWritten,
				fmt.Sprintf("node %s refused the request because the two nodes' clocks are too far apart; nothing was written", p.Name))
		case CodeVersion, CodeNoRoute:
			res.Err = publicError(models.CodeNodeVersionMismatch, p, auth.OutcomeNotWritten,
				fmt.Sprintf("node %s runs a build that does not serve this request (forwarding %s); nothing was written; it ends when every node is upgraded", p.Name, code))
		default:
			res.Err = publicError(models.CodeNodeUnreachable, p, auth.OutcomeNotWritten,
				fmt.Sprintf("node %s refused the forwarded request (%s); nothing was written", p.Name, code))
			res.Err.Details["reason"] = code
		}
		return res
	}
}

// sleep waits the pause; false when ctx ended first.
func (c *Client) sleep(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(c.pause):
		return true
	}
}

// send makes one attempt with a new envelope.
func (c *Client) send(ctx context.Context, p peerapi.Peer, req Request) (int, http.Header, []byte, error) {
	key := c.id.Key()
	if key == nil {
		return 0, nil, nil, &notSentError{errors.New("this node has no API node key")}
	}
	a, _ := c.peers.Current()
	host, _, err := net.SplitHostPort(p.Address)
	if err != nil {
		return 0, nil, nil, &notSentError{fmt.Errorf("the peer's address %q is not ip:port", p.Address)}
	}
	addr := net.JoinHostPort(host, strconv.Itoa(c.port))
	u, err := url.Parse("https://" + addr + req.URI)
	if err != nil {
		return 0, nil, nil, &notSentError{err}
	}
	spki, err := key.PublicKeyDER()
	if err != nil {
		return 0, nil, nil, &notSentError{err}
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return 0, nil, nil, &notSentError{err}
	}
	now := c.now().UnixMilli()
	env, err := Seal(Payload{V: 1, ClusterID: a.ClusterID, Origin: a.NodeID, OriginKey: base64.StdEncoding.EncodeToString(spki),
		Target: p.NodeID, UserID: req.UserID, Epoch: req.Epoch, RequestID: req.RequestID, IdempotencyKey: req.IdempotencyKey,
		SourceIP: req.SourceIP, Method: req.Method, Path: u.RequestURI(), BodySHA256: BodyHash(req.Body),
		IssuedAt: now, ExpiresAt: now + Lifetime.Milliseconds(), Nonce: hex.EncodeToString(nonce)}, key)
	if err != nil {
		return 0, nil, nil, &notSentError{err}
	}
	hr, err := http.NewRequestWithContext(ctx, req.Method, u.String(), strings.NewReader(string(req.Body)))
	if err != nil {
		return 0, nil, nil, &notSentError{err}
	}
	// Besides the envelope only these three (D22): never Cookie,
	// Authorization or Origin.
	hr.Header = http.Header{}
	hr.Header.Set(Header, env)
	if req.ContentType != "" {
		hr.Header.Set("Content-Type", req.ContentType)
	}
	if req.IdempotencyKey != "" {
		hr.Header.Set(agentproto.HeaderIdempotencyKey, req.IdempotencyKey)
	}
	if req.RequestID != "" {
		hr.Header.Set("X-Request-ID", req.RequestID)
	}
	// An empty User-Agent is not sent; with compression off no
	// Accept-Encoding is added.
	hr.Header["User-Agent"] = []string{""}
	tr := &http.Transport{
		DisableKeepAlives:  true,
		DisableCompression: true,
		TLSNextProto:       map[string]func(string, *tls.Conn) http.RoundTripper{},
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return c.dialTLS(ctx, network, addr, key, p.APIPin)
		},
	}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(hr)
	if err != nil {
		var ns *notSentError
		if errors.As(err, &ns) || refusedKey(err) {
			return 0, nil, nil, &notSentError{err}
		}
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxAnswer+1))
	if err != nil || len(body) > MaxAnswer {
		return 0, nil, nil, fmt.Errorf("the answer could not be read in full: %v", err)
	}
	return resp.StatusCode, resp.Header, body, nil
}

// dialTLS dials the peer and completes the TLS 1.3 handshake with this
// node's API node key, accepting the receiver only by the peer's API pin
// and a certificate within its validity.
func (c *Client) dialTLS(ctx context.Context, network, addr string, key *nodekey.Key, pin string) (net.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	raw, err := c.dial(dctx, network, addr)
	if err != nil {
		return nil, &notSentError{err}
	}
	conf := &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"http/1.1"},
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return key.Certificate(), nil
		},
		// Trust is the pin, not a chain or a name.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the receiver sent no certificate")
			}
			leaf := cs.PeerCertificates[0]
			if t := c.now(); t.Before(leaf.NotBefore) || t.After(leaf.NotAfter) {
				return errors.New("the receiver's certificate is expired or not yet valid")
			}
			if nodekey.PinOfCertificate(leaf) != pin {
				return errors.New("the receiver's key is not its registered API key")
			}
			return nil
		},
	}
	tc := tls.Client(raw, conf)
	if err := tc.HandshakeContext(dctx); err != nil {
		raw.Close()
		return nil, &notSentError{err}
	}
	return tc, nil
}
