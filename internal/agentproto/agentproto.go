// Package agentproto defines what skep-api and skep-agent share about the
// internal API on the agent's Unix socket: its path prefix and the request
// metadata every call must carry (ADR 0004, ADR 0006 decision 1b).
//
// The internal API is not the public API. It has its own version and makes
// no compatibility promise to third parties.
package agentproto

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"regexp"

	"github.com/greenbeanorg/skep/pkg/version"
)

// Prefix is the path prefix of version 1 of the internal API.
const Prefix = "/internal/v1"

// Request metadata headers. The actor headers are mandatory on every
// request, reads included.
const (
	HeaderActorID        = "Skep-Actor-Id"
	HeaderActorName      = "Skep-Actor-Name"
	HeaderRequestID      = "Skep-Request-Id"
	HeaderSourceIP       = "Skep-Source-Ip"
	HeaderIdempotencyKey = "Idempotency-Key"
	// HeaderOriginNode and HeaderEnvelope are set only on a request that
	// skep-api serves for another node (step 8e-1, D39): the node the
	// user's request arrived on, and the signed envelope as received.
	// Both or neither.
	HeaderOriginNode = "Skep-Origin-Node"
	HeaderEnvelope   = "Skep-Envelope"
)

// MaxEnvelope is the largest envelope the agent stores (ADR 0004,
// amendment of 2026-11-24).
const MaxEnvelope = 4096

// HeaderAgentVersion is on every response of the agent's internal API:
// the agent's build ID. skep-api refuses to work with an agent whose build
// differs from its own (503 agent_version_mismatch, ADR 0012 answer 9):
// both programs come from one package and are upgraded together, so a
// difference means only one of them was restarted.
const HeaderAgentVersion = "Skep-Agent-Version"

// BuildID is this program's build: release version and commit.
func BuildID() string {
	v := version.Get()
	return v.Version + "+" + v.Commit
}

// APIActor is the actor skep-api uses for requests it makes on its own
// behalf, such as the health check, where no user is involved.
const APIActor = "skep-api"

var (
	// nameRE is the character set the task engine accepts for actors and
	// idempotency keys.
	nameRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	// requestIDRE is the character set of request IDs in the public API.
	requestIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	// originRE is a node ID: a lower-case UUID.
	originRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// envelopeRE is the envelope's character set (D39).
	envelopeRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// Caller is the metadata of one request to the agent: who it is made for
// and where it came from. The agent records it; it cannot verify it and
// makes no authorization decision from it.
type Caller struct {
	// ActorID and ActorName identify the user the request is made for.
	// Both are mandatory.
	ActorID   string
	ActorName string
	// RequestID links the agent's records to the API's. Optional.
	RequestID string
	// SourceIP is the address of the API client. Optional.
	SourceIP string
	// IdempotencyKey is passed through for operations that accept one.
	IdempotencyKey string
	// OriginNode and Envelope are set for a request skep-api serves for
	// another node (step 8e-1, D39); the agent records both in the
	// "requested" audit entry and verifies neither.
	OriginNode string
	Envelope   string
}

// ErrNoActor is returned for a request without an actor.
var ErrNoActor = errors.New("the actor headers " + HeaderActorID + " and " + HeaderActorName + " are required")

// Validate checks every field against its character set.
func (c Caller) Validate() error {
	if c.ActorID == "" || c.ActorName == "" {
		return ErrNoActor
	}
	if !nameRE.MatchString(c.ActorID) || !nameRE.MatchString(c.ActorName) {
		return errors.New("invalid actor: use 1 to 128 characters from A-Z a-z 0-9 . _ : -")
	}
	if c.RequestID != "" && !requestIDRE.MatchString(c.RequestID) {
		return errors.New("invalid request ID")
	}
	if c.SourceIP != "" {
		if _, err := netip.ParseAddr(c.SourceIP); err != nil {
			return errors.New("invalid source address")
		}
	}
	if c.IdempotencyKey != "" && !nameRE.MatchString(c.IdempotencyKey) {
		return errors.New("invalid idempotency key: use 1 to 128 characters from A-Z a-z 0-9 . _ : -")
	}
	if (c.OriginNode == "") != (c.Envelope == "") {
		return errors.New("the origin node and the envelope are sent together or not at all")
	}
	if c.OriginNode != "" && !originRE.MatchString(c.OriginNode) {
		return errors.New("invalid origin node: a node ID is a lower-case UUID")
	}
	if c.Envelope != "" && (len(c.Envelope) > MaxEnvelope || !envelopeRE.MatchString(c.Envelope)) {
		return errors.New("invalid envelope: at most 4096 characters from A-Z a-z 0-9 . _ -")
	}
	return nil
}

// ForwardDetails returns the audit details of a forwarded request's
// origin and envelope, or nil.
func (c Caller) ForwardDetails() map[string]any {
	if c.OriginNode == "" {
		return nil
	}
	return map[string]any{"origin_node": c.OriginNode, "envelope": c.Envelope}
}

// SetHeaders writes the caller into the headers of an outgoing request.
func (c Caller) SetHeaders(h http.Header) {
	set := func(name, value string) {
		if value != "" {
			h.Set(name, value)
		}
	}
	set(HeaderActorID, c.ActorID)
	set(HeaderActorName, c.ActorName)
	set(HeaderRequestID, c.RequestID)
	set(HeaderSourceIP, c.SourceIP)
	set(HeaderIdempotencyKey, c.IdempotencyKey)
	set(HeaderOriginNode, c.OriginNode)
	set(HeaderEnvelope, c.Envelope)
}

// FromHeaders reads and validates the caller of an incoming request.
func FromHeaders(h http.Header) (Caller, error) {
	c := Caller{
		ActorID:        h.Get(HeaderActorID),
		ActorName:      h.Get(HeaderActorName),
		RequestID:      h.Get(HeaderRequestID),
		SourceIP:       h.Get(HeaderSourceIP),
		IdempotencyKey: h.Get(HeaderIdempotencyKey),
		OriginNode:     h.Get(HeaderOriginNode),
		Envelope:       h.Get(HeaderEnvelope),
	}
	if err := c.Validate(); err != nil {
		return Caller{}, err
	}
	return c, nil
}

type contextKey struct{}

// WithCaller returns ctx carrying c.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, contextKey{}, c)
}

// CallerFrom returns the caller carried by ctx, if any.
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(contextKey{}).(Caller)
	return c, ok
}
