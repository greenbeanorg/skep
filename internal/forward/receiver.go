package forward

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/httpkit"
	"github.com/greenbeanorg/skep/pkg/models"
)

// The forwarding layer's refusal codes (D23): internal, carried in
// RefusalHeader with the standard error body, never in the OpenAPI
// document.
const (
	CodeNoRoute     = "no_route"
	CodeVersion     = "version"
	CodeBadEnvelope = "bad_envelope"
	CodeNotMember   = "not_member"
	CodeWrongTarget = "wrong_target"
	CodeClock       = "clock"
	CodeBeforeStart = "before_start"
	CodeReplay      = "replay"
	CodeBusy        = "busy"
	CodeBehind      = "behind"
	CodeUserUnknown = "user_unknown"
	CodeStaleOrigin = "stale_origin"
)

// refusalStatus is the HTTP status of each refusal code. The origin reads
// the code, not the status.
var refusalStatus = map[string]int{
	CodeNoRoute:     http.StatusNotFound,
	CodeVersion:     http.StatusBadRequest,
	CodeBadEnvelope: http.StatusBadRequest,
	CodeNotMember:   http.StatusForbidden,
	CodeWrongTarget: http.StatusMisdirectedRequest,
	CodeClock:       http.StatusBadRequest,
	CodeBeforeStart: http.StatusServiceUnavailable,
	CodeReplay:      http.StatusConflict,
	CodeBusy:        http.StatusServiceUnavailable,
	CodeBehind:      http.StatusServiceUnavailable,
	CodeUserUnknown: http.StatusServiceUnavailable,
	CodeStaleOrigin: http.StatusUnauthorized,
}

// Receiver limits (D18, D25).
const (
	// MaxBody bounds a forwarded request's body.
	MaxBody = 64 << 10
	// maxFutureMillis is how far in the future an envelope may be issued
	// by the receiver's clock.
	maxFutureMillis = 15000
)

// WriteRefusal answers a refusal of the forwarding layer: the refusal
// header and the standard error body.
func WriteRefusal(w http.ResponseWriter, r *http.Request, code, message string) {
	w.Header().Set(RefusalHeader, code)
	status, ok := refusalStatus[code]
	if !ok {
		status = http.StatusBadRequest
	}
	httpkit.WriteError(w, r, status, code, message)
}

// Refusal is why the receiver refused a forwarded request. Verified says
// the envelope's signature had verified: only then is the origin known,
// and the refusal audited (D28).
type Refusal struct {
	Code     string
	Message  string
	Verified bool
	Origin   string
}

// Accepted is a forwarded request that passed every check (D24–D26).
type Accepted struct {
	Envelope *Envelope
	// User is this node's copy of the user, with this node's role.
	User auth.User
	// Body is the request body, read in full.
	Body []byte
}

// UserLookup reads a user from this node's read copy (apidb.UserStore).
type UserLookup interface {
	UserByID(ctx context.Context, id string) (auth.User, error)
}

// Receiver makes the receiver's checks, in D24–D26's order. Nothing
// reaches a handler or the agent before all have passed.
type Receiver struct {
	peers   *Peers
	users   UserLookup
	now     func() time.Time
	started int64
	nonces  *nonceCache
}

// NewReceiver returns the receiver. Its start time is now: an envelope
// issued before it is refused (B8: the replay cache is in memory).
func NewReceiver(peers *Peers, users UserLookup, now func() time.Time) *Receiver {
	if now == nil {
		now = time.Now
	}
	return &Receiver{peers: peers, users: users, now: now, started: now().UnixMilli(), nonces: newNonceCache(MaxNonces)}
}

// Check makes every check on a request that arrived on the node listener.
func (rv *Receiver) Check(r *http.Request) (*Accepted, *Refusal) {
	refuse := func(code, msg string) (*Accepted, *Refusal) { return nil, &Refusal{Code: code, Message: msg} }
	// (a) The connection's key is a usable peer's API pin now.
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return refuse(CodeNotMember, "no client certificate")
	}
	leaf := r.TLS.PeerCertificates[0]
	peer, ok := rv.peers.ByAPIPin(nodekey.PinOfCertificate(leaf))
	if !ok {
		return refuse(CodeNotMember, "this key is not the API key of a member this node accepts now")
	}
	a, _ := rv.peers.Current()
	// (b) The envelope's form, version and strict decoding.
	env, code := Parse(r.Header.Get(Header))
	if code != "" {
		return refuse(code, "the envelope is missing, not of the form v1.<payload>.<signature>, or of another version")
	}
	// (c) The key, the signature, the origin and the cluster. Nothing in
	// the payload was used before this point but its version.
	// The signature is checked with the connection's own key first; only
	// then is anything in the payload read.
	pub, _ := leaf.PublicKey.(*ecdsa.PublicKey)
	if !env.Verify(pub) {
		return refuse(CodeBadEnvelope, "the envelope's signature was not made with this connection's key")
	}
	p := env.Payload
	if p.OriginKey != base64.StdEncoding.EncodeToString(leaf.RawSubjectPublicKeyInfo) || p.Origin != peer.NodeID ||
		a.ClusterID == "" || p.ClusterID != a.ClusterID {
		return refuse(CodeBadEnvelope, "the envelope's key, origin or cluster is not this connection's")
	}
	verified := func(code, msg string) (*Accepted, *Refusal) {
		return nil, &Refusal{Code: code, Message: msg, Verified: true, Origin: p.Origin}
	}
	// (d) The target is this node.
	if p.Target != a.NodeID {
		return verified(CodeWrongTarget, "the envelope names another node than this one")
	}
	// (e) The method, the path with its query, the body and the
	// idempotency key are the request's own.
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if p.Method != r.Method || p.Path != r.RequestURI || err != nil || len(body) > MaxBody ||
		p.BodySHA256 != BodyHash(body) || p.IdempotencyKey != r.Header.Get(agentproto.HeaderIdempotencyKey) {
		return verified(CodeBadEnvelope, "the request's method, path, body or idempotency key is not the envelope's")
	}
	// D25: time and replay, by this node's clock.
	now := rv.now().UnixMilli()
	if p.ExpiresAt-p.IssuedAt > Lifetime.Milliseconds() || p.ExpiresAt < p.IssuedAt || now >= p.ExpiresAt || p.IssuedAt > now+maxFutureMillis {
		return verified(CodeClock, "the envelope has expired or was issued too far in the future by this node's clock")
	}
	if p.IssuedAt < rv.started {
		return verified(CodeBeforeStart, "the envelope was issued before this node's skep-api started")
	}
	if code := rv.nonces.add(p.Origin, p.Nonce, p.ExpiresAt, now); code != "" {
		return verified(code, "the envelope was seen before, or the replay cache is full")
	}
	// D26: the user, from this node's read copy.
	u, err := rv.users.UserByID(r.Context(), p.UserID)
	switch {
	case errors.Is(err, models.ErrNotFound):
		return verified(CodeUserUnknown, "the user is not in this node's copy of the users")
	case err != nil:
		return verified(CodeBehind, "this node's copy of the users cannot be read")
	case p.Epoch > u.CredentialEpoch:
		return verified(CodeBehind, "this node's copy of the user is behind the origin's")
	case p.Epoch < u.CredentialEpoch || u.Disabled:
		return verified(CodeStaleOrigin, "the user's credentials changed since the origin's copy, or the user is disabled")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return &Accepted{Envelope: env, User: u, Body: body}, nil
}
