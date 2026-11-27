// Package forward is skep-api's forwarding of a request to another node's
// skep-api (step 8e-1; ADR 0013 B8): the signed envelope and its checks,
// the replay cache, the copy of the peers skep-cluster reports, the client
// that sends a forwarded request, the node-to-node listener on port 8445,
// and the API node key's lifecycle. It imports neither the Raft library
// nor skep-cluster's service, socket or store packages.
package forward

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/httpkit"
)

// Protocol constants (D20, D21, D38).
const (
	// Version is the forwarding protocol's version, the envelope's
	// prefix.
	Version = "v1"
	// Header carries the envelope on a forwarded request.
	Header = "Skep-Envelope"
	// RefusalHeader marks a refusal of the forwarding layer itself (D23).
	RefusalHeader = "Skep-Forward-Refusal"
	// MaxHeader bounds the envelope header's value.
	MaxHeader = 4096
	// Lifetime is how long an envelope is valid after it was issued.
	Lifetime = 30 * time.Second
	// signPrefix is signed before the payload text.
	signPrefix = "skep-forward-v1."
)

// Payload is the envelope's content (D21). Every field is required.
type Payload struct {
	V              int    `json:"v"`
	ClusterID      string `json:"cluster_id"`
	Origin         string `json:"origin"`
	OriginKey      string `json:"origin_key"`
	Target         string `json:"target"`
	UserID         string `json:"user_id"`
	Epoch          uint64 `json:"epoch"`
	RequestID      string `json:"request_id"`
	IdempotencyKey string `json:"idempotency_key"`
	SourceIP       string `json:"source_ip"`
	Method         string `json:"method"`
	Path           string `json:"path"`
	BodySHA256     string `json:"body_sha256"`
	IssuedAt       int64  `json:"issued_at"`
	ExpiresAt      int64  `json:"expires_at"`
	Nonce          string `json:"nonce"`
}

// payloadFields is the number of JSON fields of Payload: a payload must
// name every one.
const payloadFields = 16

// Signer signs a SHA-256 digest with the origin's API node key
// (nodekey.Key).
type Signer interface {
	Sign(digest []byte) ([]byte, error)
}

// ErrTooLarge is Seal's error for an envelope over MaxHeader.
var ErrTooLarge = errors.New("the envelope would be larger than 4096 bytes")

var b64 = base64.RawURLEncoding

// signedBytes is what the signature covers.
func signedBytes(payloadText string) []byte {
	sum := sha256.Sum256([]byte(signPrefix + payloadText))
	return sum[:]
}

// Seal encodes and signs a payload: the header value
// "v1.<payload>.<signature>".
func Seal(p Payload, s Signer) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	text := b64.EncodeToString(raw)
	sig, err := s.Sign(signedBytes(text))
	if err != nil {
		return "", err
	}
	h := Version + "." + text + "." + b64.EncodeToString(sig)
	if len(h) > MaxHeader {
		return "", ErrTooLarge
	}
	return h, nil
}

// Envelope is a parsed envelope whose signature has not been verified
// yet. Nothing in Payload may be used before Verify has returned true,
// except V (D24 b; V2 of the run).
type Envelope struct {
	Payload Payload
	// Raw is the header value as received.
	Raw  string
	text string
	sig  []byte
}

// Parse reads an envelope header: its form, its version and the strict,
// bounded decoding of its payload. It returns the refusal code for a
// header it cannot read: CodeBadEnvelope (missing, too long, not of the
// form, or a payload that does not decode strictly) or CodeVersion (a
// version other than v1).
func Parse(h string) (*Envelope, string) {
	if h == "" || len(h) > MaxHeader {
		return nil, CodeBadEnvelope
	}
	parts := strings.Split(h, ".")
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		return nil, CodeBadEnvelope
	}
	if parts[0] != Version {
		return nil, CodeVersion
	}
	raw, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, CodeBadEnvelope
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return nil, CodeBadEnvelope
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != payloadFields {
		return nil, CodeBadEnvelope
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p Payload
	if err := dec.Decode(&p); err != nil || dec.More() {
		return nil, CodeBadEnvelope
	}
	if p.V != 1 {
		return nil, CodeVersion
	}
	return &Envelope{Payload: p, Raw: h, text: parts[1], sig: sig}, ""
}

// Field shapes of a payload (8e-1 fix F2).
var (
	uuidRE  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	hex64RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hex32RE = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// maxPath bounds a payload's path with its query.
const maxPath = 2048

// ValidShape reports whether every field the receiver passes on (its
// access audit, its log, the agent caller) has its shape: the IDs
// lower-case UUIDs, the request ID by httpkit's rule, the idempotency key
// empty or by agentproto's rule, the source IP empty or one address
// without a zone, a forwarded method, a path under /api/v1/ of at most
// 2048 bytes, the body hash 64 and the nonce 32 lower-case hex digits,
// both times above 0.
func (p Payload) ValidShape() bool {
	for _, id := range []string{p.ClusterID, p.Origin, p.Target, p.UserID} {
		if !uuidRE.MatchString(id) {
			return false
		}
	}
	if !httpkit.ValidRequestID(p.RequestID) || (p.IdempotencyKey != "" && !agentproto.ValidIdempotencyKey(p.IdempotencyKey)) {
		return false
	}
	if p.SourceIP != "" {
		if a, err := netip.ParseAddr(p.SourceIP); err != nil || a.Zone() != "" {
			return false
		}
	}
	switch p.Method {
	case "GET", "HEAD", "POST", "PUT", "DELETE":
	default:
		return false
	}
	return strings.HasPrefix(p.Path, "/api/v1/") && len(p.Path) <= maxPath && hex64RE.MatchString(p.BodySHA256) &&
		hex32RE.MatchString(p.Nonce) && p.IssuedAt > 0 && p.ExpiresAt > 0
}

// Verify reports whether the signature was made with pub over this
// envelope's payload.
func (e *Envelope) Verify(pub *ecdsa.PublicKey) bool {
	return pub != nil && ecdsa.VerifyASN1(pub, signedBytes(e.text), e.sig)
}

// BodyHash is the hex SHA-256 of a body, the empty body included.
func BodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
