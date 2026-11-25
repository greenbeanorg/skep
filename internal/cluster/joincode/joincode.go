// Package joincode is the text of a join request and of an admission (ADR
// 0013 B1): one line each, printed by one host and pasted on another.
//
// A line is "<kind>:<version>:<payload>:<check>", where payload is the
// unpadded base64url encoding of a JSON object and check is the first 16
// hex digits of the SHA-256 of the payload. Nothing in either is secret
// (IDs, names, addresses and key pins). A line is decoded strictly: a size
// limit, the kind and version, the check (which catches a truncated or
// mistyped paste), unknown JSON fields refused, and every value validated.
package joincode

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
)

// Kinds and the version of this build.
const (
	KindRequest   = "skep-join-request"
	KindAdmission = "skep-admission"
	Version       = "1"
	// MaxRequest and MaxAdmission bound a line.
	MaxRequest   = 2048
	MaxAdmission = 32 << 10
	checkLen     = 16
)

// Node is one node in a request or an admission. Step 8b-1 carries no API
// pin (skep-api's node-to-node key comes with step 8e).
type Node struct {
	NodeID  string `json:"node_id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	Pin     string `json:"pin"`
}

func (n Node) validate() error {
	switch {
	case !clusterstate.ValidNodeID(n.NodeID):
		return fmt.Errorf("node ID %q is not valid", n.NodeID)
	case !clusterstate.ValidName(n.Name):
		return fmt.Errorf("node name %q is not valid", n.Name)
	case !clusterstate.ValidPin(n.Pin):
		return fmt.Errorf("pin %q is not valid", n.Pin)
	}
	return clusterstate.CheckAddress(n.Address)
}

// Request is what "skep-cluster join-request" prints.
type Request struct {
	Node
}

// Admission is what "skep-cluster admit" prints: the cluster and every
// current member entry (ID, name, address, pin), and the node it admits.
type Admission struct {
	ClusterID string `json:"cluster_id"`
	// For is the admitted node, as in its request.
	For     Node   `json:"for"`
	Members []Node `json:"members"`
	// ExpiresAt is when the pending entry expires (Unix seconds, the
	// leader's clock), for the operator.
	ExpiresAt int64 `json:"expires_at"`
}

func (a Admission) validate() error {
	if !clusterstate.ValidNodeID(a.ClusterID) {
		return fmt.Errorf("cluster ID %q is not valid", a.ClusterID)
	}
	if err := a.For.validate(); err != nil {
		return fmt.Errorf("admitted node: %w", err)
	}
	if len(a.Members) == 0 || len(a.Members) > 64 {
		return fmt.Errorf("an admission lists 1 to 64 members, not %d", len(a.Members))
	}
	if err := CheckNodes(append([]Node{a.For}, a.Members...)); err != nil {
		return err
	}
	if a.ExpiresAt <= 0 {
		return errors.New("the admission has no expiry time")
	}
	return nil
}

// CheckNodes validates nodes as an admission's are: each valid, and no node
// ID, name, address or pin twice.
func CheckNodes(nodes []Node) error {
	seen := map[string]bool{}
	for _, m := range nodes {
		if err := m.validate(); err != nil {
			return fmt.Errorf("member: %w", err)
		}
		for _, k := range []string{"id:" + m.NodeID, "name:" + m.Name, "addr:" + m.Address, "pin:" + m.Pin} {
			if seen[k] {
				return fmt.Errorf("the admission lists %s twice", strings.SplitN(k, ":", 2)[1])
			}
			seen[k] = true
		}
	}
	return nil
}

// CheckPorts refuses an address whose port is not 8444 (admit and join; the
// state machine's CheckAddress does not, for the loopback tests).
func CheckPorts(nodes ...Node) error {
	for _, n := range nodes {
		if ap, err := netip.ParseAddrPort(n.Address); err != nil || ap.Port() != clusterconf.Port {
			return fmt.Errorf("address %q of %s: port %d is not skep-cluster's port %d", n.Address, n.Name, ap.Port(), clusterconf.Port)
		}
	}
	return nil
}

func encode(kind string, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(payload))
	return kind + ":" + Version + ":" + payload + ":" + hex.EncodeToString(sum[:])[:checkLen], nil
}

func decode(kind, line string, max int, v any) error {
	line = strings.TrimSpace(line)
	if len(line) > max {
		return fmt.Errorf("the %s is longer than %d characters", kind, max)
	}
	parts := strings.Split(line, ":")
	if len(parts) != 4 || parts[0] != kind {
		other := KindRequest
		if kind == KindRequest {
			other = KindAdmission
		}
		if len(parts) > 0 && parts[0] == other {
			return fmt.Errorf("this is a %s, not a %s", other, kind)
		}
		return fmt.Errorf("this is not a %s (it must start with %q)", kind, kind+":")
	}
	if parts[1] != Version {
		return fmt.Errorf("%s version %q is unknown to this build (it reads version %s)", kind, parts[1], Version)
	}
	sum := sha256.Sum256([]byte(parts[2]))
	if parts[3] != hex.EncodeToString(sum[:])[:checkLen] {
		return fmt.Errorf("the %s is damaged (its check does not match): paste it again, complete and on one line", kind)
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("the %s is damaged: %w", kind, err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("the %s cannot be read: %w", kind, err)
	}
	if dec.More() {
		return fmt.Errorf("the %s holds more than one value", kind)
	}
	return nil
}

// EncodeRequest returns the request's line.
func EncodeRequest(r Request) (string, error) {
	if err := r.validate(); err != nil {
		return "", err
	}
	return encode(KindRequest, r)
}

// ParseRequest reads and validates a request line.
func ParseRequest(line string) (Request, error) {
	var r Request
	if err := decode(KindRequest, line, MaxRequest, &r); err != nil {
		return Request{}, err
	}
	if err := r.validate(); err != nil {
		return Request{}, fmt.Errorf("the request is not valid: %w", err)
	}
	return r, nil
}

// EncodeAdmission returns the admission's line.
func EncodeAdmission(a Admission) (string, error) {
	if err := a.validate(); err != nil {
		return "", err
	}
	return encode(KindAdmission, a)
}

// ParseAdmission reads and validates an admission line.
func ParseAdmission(line string) (Admission, error) {
	var a Admission
	if err := decode(KindAdmission, line, MaxAdmission, &a); err != nil {
		return Admission{}, err
	}
	if err := a.validate(); err != nil {
		return Admission{}, fmt.Errorf("the admission is not valid: %w", err)
	}
	return a, nil
}
